package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/spf13/cobra"
	"github.com/veraison/go-cose"
)

// Release transparency (docs/RELEASE-TRANSPARENCY.md).
//
// Build provenance proves a binary was built by release.yml; it does not
// prove the release was intended. `release register` (run by release.yml)
// records each release in the public witness's append-only log: a SCITT
// Signed Statement naming the tag, the commit and every artifact's digest,
// signed by a key made for that one release and thrown away (the statement
// is attested like the binaries, so no key is held anywhere). `release
// watch` (run OFF the release infrastructure) compares three public records
// with what was intended, a tag signed by a maintainer key the monitor pins:
// the repository's tags and releases, each release's statement, and the
// witness's entries under the release subject.

const (
	releaseStatementType = "capsule-cli-release/v1"
	releaseRepo          = "action-state-group/capsule-cli"
	releaseSubject       = "pkg:github/action-state-group/capsule-cli"
	releaseIssuer        = "https://github.com/action-state-group/capsule-cli/.github/workflows/release.yml"
	releaseMaxAsset      = 1 << 20
)

// releaseStatement is the payload: JCS bytes of this object.
type releaseStatement struct {
	Type         string            `json:"type"`
	Repo         string            `json:"repo"`
	Tag          string            `json:"tag"`
	Commit       string            `json:"commit"`
	Artifacts    map[string]string `json:"artifacts"`
	StatementKey string            `json:"statement_key"`
}

func releaseFileNames(tag string) (payload, statement, registration string) {
	base := "capsulectl-" + tag + ".release"
	return base + ".json", base + ".cose", base + ".registration.json"
}

// releaseAttestationName is the keyless attestation bundle for release.json,
// made by release.yml after `release register`.
func releaseAttestationName(tag string) string {
	return "capsulectl-" + tag + ".release.sigstore.json"
}

// releaseArtifacts digests the release's binaries and attestation bundle in
// dir, never its own statement files or SHA256SUMS.
func releaseArtifacts(dir, tag string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasPrefix(name, "capsulectl-"+tag+"-") || name == "capsulectl-"+tag+".sigstore.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		out[name] = hex.EncodeToString(sum[:])
	}
	if len(out) == 0 {
		return nil, inputError("no release artifacts for " + tag + " in " + dir)
	}
	return out, nil
}

// signReleaseStatement wraps payload in a COSE_Sign1 under a fresh key,
// with the subject and issuer as CWT claims (RFC 9597).
func signReleaseStatement(payload []byte, key ed25519.PrivateKey, issuer, subject string) ([]byte, error) {
	msg := cose.NewSign1Message()
	msg.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	msg.Headers.Protected[cose.HeaderLabelContentType] = "application/json"
	msg.Headers.Protected[int64(15)] = map[any]any{int64(1): issuer, int64(2): subject}
	msg.Payload = payload
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, key)
	if err != nil {
		return nil, err
	}
	if err := msg.Sign(rand.Reader, nil, signer); err != nil {
		return nil, err
	}
	return msg.MarshalCBOR()
}

func releaseRegisterCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "register", Short: "Record a built release in the witness's transparency log (run by the release workflow)", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		dir, _ := c.Flags().GetString("dir")
		tag, _ := c.Flags().GetString("tag")
		commit, _ := c.Flags().GetString("commit")
		witnessURL, _ := c.Flags().GetString("witness")
		witnessKey, _ := c.Flags().GetString("witness-key")
		subject, _ := c.Flags().GetString("subject")
		if dir == "" || tag == "" || len(commit) != 40 {
			return inputError("release register needs --dir, --tag and the full 40-hex --commit")
		}
		authority, err := parseKeys([]string{witnessKey})
		if err != nil {
			return err
		}
		artifacts, err := releaseArtifacts(dir, tag)
		if err != nil {
			return err
		}
		public, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		payload, err := jcsOf(releaseStatement{Type: releaseStatementType, Repo: releaseRepo, Tag: tag, Commit: commit, Artifacts: artifacts, StatementKey: hex.EncodeToString(public)})
		if err != nil {
			return err
		}
		statement, err := signReleaseStatement(payload, key, releaseIssuer, subject)
		if err != nil {
			return err
		}
		reg, raw, err := registerStatement(c.Context(), witnessURL, statement)
		if err != nil {
			return err
		}
		receipt, err := base64.StdEncoding.DecodeString(reg.ReceiptB64)
		if err != nil {
			return errors.New("the witness returned an unreadable receipt")
		}
		if err := verifyStatementReceipt(statement, receipt, reg, authority[0]); err != nil {
			return fmt.Errorf("the witness's receipt does not verify: %w", err)
		}
		payloadName, statementName, registrationName := releaseFileNames(tag)
		for name, body := range map[string][]byte{payloadName: payload, statementName: statement, registrationName: raw} {
			if err := atomicFile(filepath.Join(dir, name), body, false); err != nil {
				return err
			}
		}
		return output(c, map[string]any{"tag": tag, "commit": commit, "artifacts": artifacts, "subject": subject,
			"entry_hash": reg.EntryHash, "leaf_index": reg.LeafIndex, "tree_size": reg.TreeSize,
			"files": []string{payloadName, statementName, registrationName}})
	}}
	cmd.Flags().String("dir", "", "Directory holding the built release files")
	cmd.Flags().String("tag", "", "The release tag")
	cmd.Flags().String("commit", "", "The tagged commit (40 hex)")
	cmd.Flags().String("witness", dealDefaultWitness, "The SCITT transparency service")
	cmd.Flags().String("witness-key", dealDefaultWitnessKey, "The witness's pinned Ed25519 authority key (hex)")
	cmd.Flags().String("subject", releaseSubject, "The statement's subject (CWT sub)")
	return cmd
}

// jcsOf is the RFC 8785 bytes of v, by way of its JSON form.
func jcsOf(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	return canonical.JCS(generic)
}

var releaseHTTP = &http.Client{Timeout: 30 * time.Second}

func registerStatement(ctx context.Context, witnessURL string, statement []byte) (statementRegistration, []byte, error) {
	body, _ := json.Marshal(map[string]string{"signed_statement_b64": base64.StdEncoding.EncodeToString(statement)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(witnessURL, "/")+"/transparency/register-statement", bytes.NewReader(body))
	if err != nil {
		return statementRegistration{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := releaseHTTP.Do(req)
	if err != nil {
		return statementRegistration{}, nil, fmt.Errorf("the witness was not reached: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, releaseMaxAsset))
	if err != nil {
		return statementRegistration{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return statementRegistration{}, nil, fmt.Errorf("the witness refused the statement: HTTP %d", resp.StatusCode)
	}
	var reg statementRegistration
	if err := json.Unmarshal(raw, &reg); err != nil {
		return statementRegistration{}, nil, errors.New("the witness's answer is not a registration")
	}
	return reg, raw, nil
}

// --- the monitor ---------------------------------------------------------

type releaseWatchConfig struct {
	repo, api, witness, subject string
	allowedSigners              string
	knownUnsigned               map[string]bool
	authority                   ed25519.PublicKey
	gh, trustedRoot             string
	grace                       time.Duration
}

// releaseNow is the monitor's clock; a variable so tests can move it.
var releaseNow = time.Now

// Limits on what the monitor reads; past them it fails closed. Variables so
// tests can reach them.
var (
	releaseMaxPages          = 50
	releaseMaxListBody int64 = 16 << 20
)

// releaseAttestationCheck verifies that file's keyless attestation (bundle)
// was made by the release workflow running at refs/tags/<tag> on commit. A
// variable so tests can stand in for gh and Sigstore.
var releaseAttestationCheck = func(ctx context.Context, cfg releaseWatchConfig, file, bundle, tag, commit string) error {
	args := []string{"attestation", "verify", file, "--bundle", bundle, "--repo", cfg.repo,
		"--cert-identity", "https://github.com/" + cfg.repo + "/.github/workflows/release.yml@refs/tags/" + tag,
		"--source-ref", "refs/tags/" + tag, "--source-digest", commit, "--deny-self-hosted-runners"}
	if cfg.trustedRoot != "" {
		args = append(args, "--custom-trusted-root", cfg.trustedRoot)
	}
	cmd := exec.CommandContext(ctx, cfg.gh, args...)
	// No account is used or needed: the bundle carries the attestation.
	cmd.Env = append(os.Environ(), "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="+os.TempDir()+"/capsulectl-release-watch-gh")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// verifyReleaseStatementSignature checks the statement's COSE signature
// under the one-release key its payload names, and its CWT issuer and
// subject. On its own this proves only that the statement is internally
// consistent (anyone can make one): the attestation of release.json is what
// ties that key to the release workflow.
func verifyReleaseStatementSignature(statement []byte, st releaseStatement, subject string) error {
	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(statement); err != nil {
		return errors.New("not a COSE_Sign1")
	}
	if alg, err := msg.Headers.Protected.Algorithm(); err != nil || alg != cose.AlgorithmEdDSA {
		return errors.New("not EdDSA")
	}
	claims, ok := receiptLookup(msg.Headers.Protected, 15)
	cwt, isMap := claims.(map[any]any)
	if !ok || !isMap {
		return errors.New("no CWT claims")
	}
	iss, _ := receiptLookup(cwt, 1)
	sub, _ := receiptLookup(cwt, 2)
	if iss != releaseIssuer || sub != subject {
		return fmt.Errorf("issuer %v and subject %v are not the release workflow's", iss, sub)
	}
	key, err := hex.DecodeString(st.StatementKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("its payload names no Ed25519 statement key")
	}
	verifier, err := cose.NewVerifier(cose.AlgorithmEdDSA, ed25519.PublicKey(key))
	if err != nil {
		return err
	}
	if err := msg.Verify(nil, verifier); err != nil {
		return errors.New("the signature does not verify under its statement key")
	}
	return nil
}

type githubTag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
		URL    string `json:"browser_download_url"`
	} `json:"assets"`
}

func releaseGetJSON(ctx context.Context, rawURL string, into any) error {
	raw, err := releaseGetBytes(ctx, rawURL)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func releaseGetBytes(ctx context.Context, rawURL string) ([]byte, error) {
	raw, _, err := releaseGet(ctx, rawURL, releaseMaxAsset)
	return raw, err
}

// releaseGetAll reads every page of a GitHub list, following the Link
// header's rel="next", and fails closed rather than check a partial list.
func releaseGetAll[T any](ctx context.Context, rawURL string) ([]T, error) {
	var all []T
	for page := 0; rawURL != ""; page++ {
		if page == releaseMaxPages {
			return nil, fmt.Errorf("more than %d pages at %s: refusing to check a partial list", releaseMaxPages, rawURL)
		}
		raw, header, err := releaseGet(ctx, rawURL, releaseMaxListBody)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("GET %s: not a list", rawURL)
		}
		all = append(all, items...)
		rawURL = nextLink(header.Get("Link"))
	}
	return all, nil
}

// nextLink is the rel="next" target of an RFC 8288 Link header, or "".
func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// releaseGet reads at most limit bytes and fails, rather than truncate, on
// a longer body.
func releaseGet(ctx context.Context, rawURL string, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	// A token only raises the rate limit; the monitor never needs one.
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && strings.HasPrefix(rawURL, "https://api.github.com/") {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := releaseHTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(raw)) > limit {
		return nil, nil, fmt.Errorf("GET %s: the response is over %d bytes: refusing to check part of it", rawURL, limit)
	}
	return raw, resp.Header, nil
}

// tagSignedBy reports the allowed signer whose SSH signature verifies the
// tag, or "" and why not, and the tagger time from the signed tag object.
func (cfg releaseWatchConfig) tagSignedBy(ctx context.Context, tag string) (string, string, time.Time, error) {
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := releaseGetJSON(ctx, cfg.api+"/repos/"+cfg.repo+"/git/ref/tags/"+url.PathEscape(tag), &ref); err != nil {
		return "", "", time.Time{}, err
	}
	if ref.Object.Type != "tag" {
		return "", "a lightweight tag carries no signature", time.Time{}, nil
	}
	var obj struct {
		Verification struct {
			Signature string `json:"signature"`
			Payload   string `json:"payload"`
		} `json:"verification"`
	}
	if err := releaseGetJSON(ctx, cfg.api+"/repos/"+cfg.repo+"/git/tags/"+ref.Object.SHA, &obj); err != nil {
		return "", "", time.Time{}, err
	}
	sig := obj.Verification.Signature
	switch {
	case sig == "":
		return "", "unsigned", time.Time{}, nil
	case !strings.Contains(sig, "BEGIN SSH SIGNATURE"):
		return "", "not an SSH signature", time.Time{}, nil
	}
	signer, why, err := verifySSHSignature(ctx, cfg.allowedSigners, []byte(obj.Verification.Payload), []byte(sig))
	return signer, why, taggerTime(obj.Verification.Payload), err
}

// taggerTime reads the time from a tag object's "tagger Name <email>
// <unix seconds> <zone>" line, which the tag's signature covers.
func taggerTime(payload string) time.Time {
	for _, line := range strings.Split(payload, "\n") {
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "tagger "); ok {
			fields := strings.Fields(rest[strings.LastIndex(rest, ">")+1:])
			if len(fields) >= 1 {
				var secs int64
				if _, err := fmt.Sscan(fields[0], &secs); err == nil {
					return time.Unix(secs, 0)
				}
			}
		}
	}
	return time.Time{}
}

// verifySSHSignature checks a git SSH signature (namespace "git") against an
// allowed_signers file with ssh-keygen, and names the signer.
func verifySSHSignature(ctx context.Context, allowed string, payload, sig []byte) (string, string, error) {
	dir, err := os.MkdirTemp("", "release-watch-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	sigPath := filepath.Join(dir, "tag.sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		return "", "", err
	}
	find := exec.CommandContext(ctx, "ssh-keygen", "-Y", "find-principals", "-f", allowed, "-s", sigPath)
	principals, err := find.Output()
	if err != nil {
		return "", "signed by a key not in the allowed signers", nil
	}
	for _, principal := range strings.Fields(string(principals)) {
		verify := exec.CommandContext(ctx, "ssh-keygen", "-Y", "verify", "-f", allowed, "-I", principal, "-n", "git", "-s", sigPath)
		verify.Stdin = bytes.NewReader(payload)
		if verify.Run() == nil {
			return principal, "", nil
		}
	}
	return "", "the signature does not verify", nil
}

func releaseWatch(ctx context.Context, cfg releaseWatchConfig) (map[string]any, error) {
	var alarms []string
	alarm := func(format string, args ...any) { alarms = append(alarms, fmt.Sprintf(format, args...)) }

	tags, err := releaseGetAll[githubTag](ctx, cfg.api+"/repos/"+cfg.repo+"/tags?per_page=100")
	if err != nil {
		return nil, err
	}
	taggedAt := map[string]time.Time{}
	commitOf := map[string]string{}
	intended := map[string]string{} // tag -> who signed, or "known exception"
	for _, t := range tags {
		if !strings.HasPrefix(t.Name, "v") {
			continue
		}
		commitOf[t.Name] = t.Commit.SHA
		if cfg.knownUnsigned[t.Name] {
			intended[t.Name] = "known exception"
			continue
		}
		signer, why, at, err := cfg.tagSignedBy(ctx, t.Name)
		if err != nil {
			return nil, err
		}
		taggedAt[t.Name] = at
		if signer == "" {
			alarm("unintended tag %s: %s", t.Name, why)
			continue
		}
		intended[t.Name] = signer
	}

	releases, err := releaseGetAll[githubRelease](ctx, cfg.api+"/repos/"+cfg.repo+"/releases?per_page=100")
	if err != nil {
		return nil, err
	}
	released := map[string]bool{}
	for _, r := range releases {
		released[r.TagName] = true
	}
	for tag := range intended {
		if released[tag] || cfg.knownUnsigned[tag] {
			continue
		}
		// A signed tag is a promise of a release: one never published, or
		// deleted, must not pass quietly.
		if at := taggedAt[tag]; at.IsZero() || releaseNow().Sub(at) > cfg.grace {
			alarm("intended tag %s has no release (tagged %s; grace %s)", tag, at.UTC().Format(time.RFC3339), cfg.grace)
		}
	}
	expected := map[string]string{} // entry hash -> tag
	checked := []string{}
	for _, r := range releases {
		tag := r.TagName
		if _, ok := intended[tag]; !ok {
			alarm("unintended release %s: its tag is not signed by an allowed signer", tag)
			continue
		}
		payloadName, statementName, registrationName := releaseFileNames(tag)
		assets := map[string]string{}
		urls := map[string]string{}
		for _, a := range r.Assets {
			assets[a.Name] = strings.TrimPrefix(a.Digest, "sha256:")
			urls[a.Name] = a.URL
		}
		if urls[payloadName] == "" || urls[statementName] == "" || urls[registrationName] == "" {
			if !cfg.knownUnsigned[tag] {
				alarm("release %s has no release statement", tag)
			}
			continue
		}
		payload, err1 := releaseGetBytes(ctx, urls[payloadName])
		statement, err2 := releaseGetBytes(ctx, urls[statementName])
		regRaw, err3 := releaseGetBytes(ctx, urls[registrationName])
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, err
		}
		var attestation []byte
		if u := urls[releaseAttestationName(tag)]; u != "" {
			var err error
			if attestation, err = releaseGetBytes(ctx, u); err != nil {
				return nil, err
			}
		}
		var st releaseStatement
		var reg statementRegistration
		if json.Unmarshal(payload, &st) != nil || json.Unmarshal(regRaw, &reg) != nil {
			alarm("release %s: its statement files do not parse", tag)
			continue
		}
		var msg cose.Sign1Message
		if msg.UnmarshalCBOR(statement) != nil || !bytes.Equal(msg.Payload, payload) {
			alarm("release %s: the signed statement does not carry its release.json", tag)
			continue
		}
		if st.Type != releaseStatementType || st.Repo != cfg.repo || st.Tag != tag || st.Commit != commitOf[tag] {
			alarm("release %s: its statement names %s %s at %s, not this tag's commit %s", tag, st.Repo, st.Tag, st.Commit, commitOf[tag])
			continue
		}
		if err := verifyReleaseStatementSignature(statement, st, cfg.subject); err != nil {
			alarm("release %s: its statement's signature does not verify: %v", tag, err)
			continue
		}
		if attestation == nil {
			alarm("release %s: release.json has no attestation (%s), so nothing ties its statement to the release workflow", tag, releaseAttestationName(tag))
			continue
		}
		if err := checkReleaseAttestation(ctx, cfg, payload, attestation, tag); err != nil {
			alarm("release %s: release.json's attestation does not verify as the release workflow at refs/tags/%s on %s: %v", tag, tag, st.Commit, err)
			continue
		}
		for name, digest := range assets {
			if strings.HasPrefix(name, "capsulectl-"+tag+"-") || name == "capsulectl-"+tag+".sigstore.json" {
				if st.Artifacts[name] != digest {
					alarm("release %s: asset %s (sha256 %s) is not what its statement records", tag, name, digest)
				}
			}
		}
		for name := range st.Artifacts {
			if _, ok := assets[name]; !ok {
				alarm("release %s: the statement records %s, which the release does not have", tag, name)
			}
		}
		receipt, err := base64.StdEncoding.DecodeString(reg.ReceiptB64)
		if err != nil || verifyStatementReceipt(statement, receipt, reg, cfg.authority) != nil {
			alarm("release %s: its witness receipt does not verify", tag)
			continue
		}
		expected[reg.EntryHash] = tag
		checked = append(checked, tag)
	}

	var logged struct {
		Entries []struct {
			EntryHash string `json:"entry_hash"`
			Payload   string `json:"capsule_id_digest"`
		} `json:"entries"`
		Next       any `json:"next"`
		NextCursor any `json:"next_cursor"`
		Cursor     any `json:"cursor"`
	}
	// The witness returns every entry under a subject in one response. If
	// it ever pages, this fails closed rather than check part of the log.
	statementsURL := strings.TrimRight(cfg.witness, "/") + "/transparency/statements?subject=" + url.QueryEscape(cfg.subject)
	raw, header, err := releaseGet(ctx, statementsURL, releaseMaxListBody)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &logged); err != nil {
		return nil, fmt.Errorf("GET %s: not a statement list", statementsURL)
	}
	if nextLink(header.Get("Link")) != "" || logged.Next != nil || logged.NextCursor != nil || logged.Cursor != nil {
		return nil, fmt.Errorf("GET %s: the witness paged its answer: refusing to check part of the log", statementsURL)
	}
	seen := map[string]bool{}
	for _, e := range logged.Entries {
		seen[e.EntryHash] = true
		if _, ok := expected[e.EntryHash]; ok {
			continue
		}
		claim := ""
		if raw, err := hex.DecodeString(e.Payload); err == nil {
			var st releaseStatement
			if json.Unmarshal(raw, &st) == nil && st.Tag != "" {
				claim = fmt.Sprintf(" (it claims %s at %s)", st.Tag, st.Commit)
			}
		}
		alarm("statement under %s that does not verify as one of our releases: entry %s%s (registration is open, so anyone can log under any subject)", cfg.subject, e.EntryHash, claim)
	}
	for hash, tag := range expected {
		if !seen[hash] {
			alarm("release %s: its statement is not in the witness's log under %s", tag, cfg.subject)
		}
	}

	sort.Strings(alarms)
	sort.Strings(checked)
	names := make([]string, 0, len(intended))
	for t := range intended {
		names = append(names, t)
	}
	sort.Strings(names)
	state := "ok"
	if len(alarms) > 0 {
		state = "alarm"
	}
	if alarms == nil {
		alarms = []string{}
	}
	return map[string]any{"state": state, "repo": cfg.repo, "subject": cfg.subject, "intended_tags": names,
		"releases_checked": checked, "statements_in_log": len(logged.Entries), "alarms": alarms}, nil
}

// checkReleaseAttestation runs releaseAttestationCheck on payload and its
// bundle, written to a scratch directory.
func checkReleaseAttestation(ctx context.Context, cfg releaseWatchConfig, payload, bundle []byte, tag string) error {
	var st releaseStatement
	if err := json.Unmarshal(payload, &st); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "release-watch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	payloadName, _, _ := releaseFileNames(tag)
	file, bundleFile := filepath.Join(dir, payloadName), filepath.Join(dir, releaseAttestationName(tag))
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(bundleFile, bundle, 0o600); err != nil {
		return err
	}
	return releaseAttestationCheck(ctx, cfg, file, bundleFile, tag, st.Commit)
}

func releaseWatchCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "watch", Short: "Compare the repository's releases and the transparency log with the intended releases (maintainer-signed tags); run it off the release infrastructure", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		cfg := releaseWatchConfig{knownUnsigned: map[string]bool{}}
		cfg.repo, _ = c.Flags().GetString("repo")
		cfg.api, _ = c.Flags().GetString("github-api")
		cfg.witness, _ = c.Flags().GetString("witness")
		cfg.subject, _ = c.Flags().GetString("subject")
		cfg.allowedSigners, _ = c.Flags().GetString("allowed-signers")
		key, _ := c.Flags().GetString("witness-key")
		known, _ := c.Flags().GetStringSlice("known-unsigned")
		cfg.gh, _ = c.Flags().GetString("gh")
		cfg.trustedRoot, _ = c.Flags().GetString("trusted-root")
		cfg.grace, _ = c.Flags().GetDuration("release-grace")
		if cfg.allowedSigners == "" {
			return inputError("--allowed-signers is required: the maintainer keys whose signed tags mark an intended release (ssh allowed_signers format)")
		}
		if _, err := os.Stat(cfg.allowedSigners); err != nil {
			return inputError("--allowed-signers: cannot read " + cfg.allowedSigners)
		}
		authority, err := parseKeys([]string{key})
		if err != nil {
			return err
		}
		cfg.authority = authority[0]
		for _, t := range known {
			cfg.knownUnsigned[strings.TrimSpace(t)] = true
		}
		out, err := releaseWatch(c.Context(), cfg)
		if err != nil {
			return err
		}
		if err := output(c, out); err != nil {
			return err
		}
		if out["state"] != "ok" {
			return hint(ErrAlarm, fmt.Sprintf("%d release alarm(s): see alarms", len(out["alarms"].([]string))))
		}
		return nil
	}}
	cmd.Flags().String("repo", releaseRepo, "The repository whose releases to check")
	cmd.Flags().String("github-api", "https://api.github.com", "The GitHub API base")
	cmd.Flags().String("witness", dealDefaultWitness, "The SCITT transparency service")
	cmd.Flags().String("witness-key", dealDefaultWitnessKey, "The witness's pinned Ed25519 authority key (hex)")
	cmd.Flags().String("subject", releaseSubject, "The release statements' subject")
	cmd.Flags().String("allowed-signers", "", "ssh allowed_signers file naming the maintainer keys whose signed tags are intended releases (kept with the monitor, never in the repository)")
	cmd.Flags().String("gh", "gh", "The gh CLI used to verify release.json's attestation bundle (no account is used)")
	cmd.Flags().String("trusted-root", "", "Optional Sigstore trusted_root.jsonl, passed to gh as --custom-trusted-root for an offline check")
	cmd.Flags().Duration("release-grace", 2*time.Hour, "How long after a signed tag its release may take before its absence is an alarm")
	cmd.Flags().StringSlice("known-unsigned", nil, "Tags made before signing began, accepted as known exceptions (comma-separated)")
	return cmd
}

func releaseCommands() *cobra.Command {
	release := &cobra.Command{Use: "release", Short: "Record releases in a transparency log, and watch that log against the intended releases"}
	release.AddCommand(releaseRegisterCommand(), releaseWatchCommand())
	return release
}
