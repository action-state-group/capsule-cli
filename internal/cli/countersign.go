// Package cli: countersign verbs work against any countersigning service and
// carry no account logic -- the wire contract they speak (countersignAPI, the
// submission body, the directory shape) is documented here so a third party
// can interoperate without reading capsule-engine (or any other private repo).
package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/spf13/cobra"
)

// decodeBundleJSON decodes an Evidence Bundle into a generic map with
// UseNumber, matching the convention getCapsule and aacbundle.DecodeFragment
// already use for map[string]interface{} targets: without it, encoding/json
// decodes every JSON number as float64, which aacbundle's JCS canonicalizer
// (and BundleDigest, which it backs) rejects outright.
func decodeBundleJSON(raw []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var bundle map[string]interface{}
	if err := decoder.Decode(&bundle); err != nil {
		return nil, inputError("invalid bundle JSON")
	}
	return bundle, nil
}

// countersignAPI names the wire type of the countersignatures[] entries this
// CLI produces and verifies. The base Evidence Bundle draft (-00, merged)
// reserves the countersignatures[] slot as future scope behind a registered
// "type" field (initial registered value "cose-sign1"; a current verifier
// MUST surface any entry as unverified). "countersign/v1" is this CLI's own
// type, ahead of the richer entry-shape proposal's ratification
// (action-state-ops/spec/inbox.md [bundle-countersignatures-entry-and-directory],
// filed 2026-09-16, not yet merged into the spec text) -- running code ahead
// of the posted draft, same as the bilateral mechanism before it. Any entry of
// a different type (including "cose-sign1") is reported "unverified" here,
// never rejected: this core never claims authority over a type it does not
// define.
const countersignAPI = "countersign/v1"

// defaultCountersignerDirectoryURL is the one configurable default named in
// the task brief ("URL configurable; ours is one value"). The directory
// itself (checkpointed-local-log's witnesses.json, extended with a
// countersigners[] array) is a separate, not-yet-shipped deliverable
// (spec/inbox [bundle-countersignatures-entry-and-directory] item 3, and the
// parked [witness-directory-v1] note both place it in checkpointed-local-log,
// never on the agentactioncapsule.org domain or inside capsule-anchor). This
// default is a documented placeholder for that eventual location and MUST be
// overridable via --directory until the file exists on that path.
const defaultCountersignerDirectoryURL = "https://raw.githubusercontent.com/action-state-group/checkpointed-local-log/main/witnesses.json"

// CountersignSigner identifies the party that made a countersignature.
type CountersignSigner struct {
	ID    string `json:"id"`
	KeyID string `json:"key_id"`
}

// CountersignCheck is one named recomputation result. The countersigning
// service, not this CLI, is responsible for keeping result words to the
// five-word, non-rating vocabulary the spec item requires; this CLI carries
// whatever it is given and never invents or edits a result.
type CountersignCheck struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

// CountersignScope names what a countersignature attests over.
type CountersignScope struct {
	LedgerID     string      `json:"ledger_id"`
	Period       string      `json:"period,omitempty"`
	ClosureDepth json.Number `json:"closure_depth,omitempty"`
}

// CountersignStatement is the recomputation statement accompanying a
// countersignature: what was recomputed, and with what result.
type CountersignStatement struct {
	Checks       []CountersignCheck `json:"checks"`
	RecomputedAt string             `json:"recomputed_at"`
	Scope        CountersignScope   `json:"scope"`
}

// CountersignatureEntry is one element of bundle["countersignatures"], per
// spec/inbox [bundle-countersignatures-entry-and-directory]: {signer, over,
// statement, signature, receipt?}. The signature verifies over the bundle
// digest (the "over" field, UTF-8 bytes of its 64-hex-character form) -- not
// over the statement, which accompanies the signature but is not what is
// signed (the entry shape's own definition sentence). "independent" is never
// carried on the wire: a self-countersignature is well-formed and it is the
// verifier's job to render it as not independent by comparing the signer's
// key against the bundle producer's trusted keys, never the countersigner's
// own self-report.
type CountersignatureEntry struct {
	Type      string               `json:"type"`
	Signer    CountersignSigner    `json:"signer"`
	Over      string               `json:"over"`
	Statement CountersignStatement `json:"statement"`
	Signature string               `json:"signature"`
	Receipt   json.RawMessage      `json:"receipt,omitempty"`
}

// countersignSubmission is the request body countersign request POSTs to
// --service: the withheld bundle, the requested attestation window, and the
// requester's own signature over the bundle digest -- proof to the service of
// who is asking, distinct from (and never stored as) a countersignatures[]
// entry.
type countersignSubmission struct {
	Bundle             map[string]interface{} `json:"bundle"`
	Window             string                 `json:"window"`
	Requester          CountersignSigner      `json:"requester"`
	RequesterSignature string                 `json:"requester_signature"`
}

// countersignSubmissionResponse is the service's reply: the new
// countersignatures[] entry or entries to attach to the bundle file.
type countersignSubmissionResponse struct {
	Countersignatures []CountersignatureEntry `json:"countersignatures"`
}

// countersignerDirectoryRow is one entry of the countersigner directory's
// countersigners[] array (spec/inbox item 3: operator, endpoint, key ids,
// statement types issued, since, independent_of[]).
type countersignerDirectoryRow struct {
	Name           string   `json:"name"`
	KeyIDs         []string `json:"key_ids"`
	Endpoint       string   `json:"endpoint,omitempty"`
	StatementTypes []string `json:"statement_types_issued,omitempty"`
	Since          string   `json:"since,omitempty"`
	IndependentOf  []string `json:"independent_of,omitempty"`
}

type countersignerDirectory struct {
	Countersigners []countersignerDirectoryRow `json:"countersigners"`
}

// signBundleDigest signs the UTF-8 bytes of a bundle digest (a 64-lowercase-
// hex-character string) with an Ed25519 key. AGENTS.md asks that AAC
// digest/signature rules not be reimplemented; this is not one of those --
// bundle-digest signing has no existing implementation anywhere in this
// stack, so this plain Ed25519-over-the-hex-string scheme is this CLI's own,
// documented here pending the entry shape's ratification.
func signBundleDigest(key ed25519.PrivateKey, digestHex string) string {
	return hex.EncodeToString(ed25519.Sign(key, []byte(digestHex)))
}

// verifyDigestSignature checks an Ed25519 signature (hex) by a signer key
// (hex) over a bundle digest (hex). Every failure is reported distinctly so a
// caller can tell a malformed key from a malformed signature from a genuine
// verification failure.
func verifyDigestSignature(signerKeyHex, digestHex, signatureHex string) error {
	key, err := hex.DecodeString(signerKeyHex)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("signer key_id is not a 32-byte Ed25519 public key in hex")
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("signature is not a 64-byte Ed25519 signature in hex")
	}
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(digestHex), signature) {
		return errors.New("signature does not verify over the bundle digest")
	}
	return nil
}

// validateServiceURL requires an absolute HTTPS URL without embedded
// credentials or a fragment, mirroring the same rule checkpoint.go's
// serviceID applies to the checkpoint witness endpoint: a countersign service
// is a sensitive remote submission and never takes plaintext or a URL a
// caller could confuse with a fragment-carried bundle.
func validateServiceURL(raw string) (string, error) {
	if raw == "" {
		return "", inputError("--service is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", inputError("--service must be an absolute HTTPS URL without embedded credentials or a fragment")
	}
	return raw, nil
}

// validateDirectoryURL applies the same HTTPS-only rule to the countersigner
// directory fetch: it is a second outbound network call this CLI makes, and
// there is no reason to hold it to a weaker bar than --service.
func validateDirectoryURL(raw string) (string, error) {
	if raw == "" {
		raw = defaultCountersignerDirectoryURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", inputError("--directory must be an absolute HTTPS URL without embedded credentials or a fragment")
	}
	return raw, nil
}

const maxCountersignResponse = 1 << 20

// postJSON POSTs a JSON body and decodes a JSON response, rejecting unknown
// fields and any non-200 status the same way the rest of the CLI treats an
// untrusted network response: named failures, no pass-through.
func postJSON(ctx context.Context, client *http.Client, target string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCountersignResponse+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned status %d", target, resp.StatusCode)
	}
	if len(raw) > maxCountersignResponse {
		return fmt.Errorf("%s response exceeds size limit", target)
	}
	return decodeJSON(raw, out)
}

// getJSON is postJSON's read-only counterpart, used for the directory fetch.
func getJSON(ctx context.Context, client *http.Client, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCountersignResponse+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned status %d", target, resp.StatusCode)
	}
	if len(raw) > maxCountersignResponse {
		return fmt.Errorf("%s response exceeds size limit", target)
	}
	return decodeJSON(raw, out)
}

// buildCountersignSubmission signs the bundle digest with the requester's own
// key and assembles the request body countersign request submits.
func buildCountersignSubmission(bundle map[string]interface{}, window, requesterID string, key ed25519.PrivateKey) (countersignSubmission, string, error) {
	digest, err := aacbundle.BundleDigest(bundle)
	if err != nil {
		return countersignSubmission{}, "", err
	}
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return countersignSubmission{}, "", inputError("invalid signing key")
	}
	submission := countersignSubmission{
		Bundle:             bundle,
		Window:             window,
		Requester:          CountersignSigner{ID: requesterID, KeyID: hex.EncodeToString(public)},
		RequesterSignature: signBundleDigest(key, digest),
	}
	return submission, digest, nil
}

// requestCountersignatures submits a signed submission to a countersign
// service and returns its countersignatures[] entries, each already checked
// to verify over the submitted digest -- a response entry that does not
// verify, or that names a different digest, is refused here rather than
// attached to the bundle file.
func requestCountersignatures(ctx context.Context, client *http.Client, service string, submission countersignSubmission, digest string) ([]CountersignatureEntry, error) {
	var response countersignSubmissionResponse
	if err := postJSON(ctx, client, service, submission, &response); err != nil {
		return nil, err
	}
	if len(response.Countersignatures) == 0 {
		return nil, errors.New("countersign service returned no countersignatures[] entry")
	}
	for i, entry := range response.Countersignatures {
		if entry.Over != digest {
			return nil, fmt.Errorf("countersignatures[%d] signs a different bundle digest", i)
		}
		if err := verifyDigestSignature(entry.Signer.KeyID, digest, entry.Signature); err != nil {
			return nil, fmt.Errorf("countersignatures[%d]: %w", i, err)
		}
	}
	return response.Countersignatures, nil
}

// attachCountersignatures appends entries to bundle["countersignatures"],
// preserving whatever was already there.
func attachCountersignatures(bundle map[string]interface{}, entries []CountersignatureEntry) error {
	existing, _ := bundle["countersignatures"].([]interface{})
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		var value interface{}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		existing = append(existing, value)
	}
	bundle["countersignatures"] = existing
	return nil
}

// fetchCountersignerDirectory retrieves and parses the countersigner
// directory from --directory (or the default).
func fetchCountersignerDirectory(ctx context.Context, client *http.Client, directoryURL string) (countersignerDirectory, error) {
	var dir countersignerDirectory
	if err := getJSON(ctx, client, directoryURL, &dir); err != nil {
		return countersignerDirectory{}, fmt.Errorf("fetching countersigner directory: %w", err)
	}
	return dir, nil
}

// resolveSigner looks up a signer's key_id in the directory, matching against
// every row's key_ids (a countersigner may rotate through several).
func resolveSigner(dir countersignerDirectory, keyHex string) (countersignerDirectoryRow, bool) {
	for _, row := range dir.Countersigners {
		for _, key := range row.KeyIDs {
			if strings.EqualFold(key, keyHex) {
				return row, true
			}
		}
	}
	return countersignerDirectoryRow{}, false
}

// countersignatureReport is one countersignatures[] entry's verify outcome:
// the three named states (not_independent / resolved / the hollow "none" case
// the caller renders when the list is empty), plus "unresolved_signer" for an
// entry whose signer verifies but is absent from the directory, and
// "unverified" for a wire type this CLI does not define (including the -00
// spec's own reserved "cose-sign1").
type countersignatureReport struct {
	State        string             `json:"state"`
	Signer       CountersignSigner  `json:"signer"`
	Independent  *bool              `json:"independent,omitempty"`
	SignerName   string             `json:"signer_name,omitempty"`
	Checks       []CountersignCheck `json:"checks,omitempty"`
	RecomputedAt string             `json:"recomputed_at,omitempty"`
}

// verifyCountersignatures is countersign verify's testable core: it verifies
// every countersignatures[] entry's signature over the bundle digest, flags a
// self-countersignature as not independent by comparing against the bundle
// producer's trusted keys, and resolves every other signer against the
// countersigner directory. A tampered entry (bad signature, or an "over" that
// does not match the recomputed digest) is a hard failure -- verification
// must be able to fail, per the queue protocol's §7 rule -- so this returns an
// error rather than a report for that entry.
func verifyCountersignatures(ctx context.Context, client *http.Client, directoryURL string, bundle map[string]interface{}, trustedProducerKeys []ed25519.PublicKey) (string, []countersignatureReport, string, error) {
	digest, err := aacbundle.BundleDigest(bundle)
	if err != nil {
		return "", nil, "", err
	}
	rawEntries, _ := bundle["countersignatures"].([]interface{})
	if len(rawEntries) == 0 {
		return digest, nil, "none", nil
	}
	var directory *countersignerDirectory
	var reports []countersignatureReport
	summary := "none"
	rank := map[string]int{"none": 0, "unverified": 1, "unresolved_signer": 2, "not_independent": 3, "resolved": 4}
	for i, raw := range rawEntries {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return "", nil, "", fmt.Errorf("countersignatures[%d] is malformed: %w", i, err)
		}
		var entry CountersignatureEntry
		if err := json.Unmarshal(encoded, &entry); err != nil {
			return "", nil, "", fmt.Errorf("countersignatures[%d] is malformed: %w", i, err)
		}
		if entry.Type != countersignAPI {
			// A reserved slot this core does not define: report it, verify
			// nothing about it, never fail the bundle for it (the -00 spec's
			// own rule for the "cose-sign1" type).
			reports = append(reports, countersignatureReport{State: "unverified", Signer: entry.Signer})
			continue
		}
		if entry.Over != digest {
			return "", nil, "", fmt.Errorf("countersignatures[%d] signs a different bundle digest", i)
		}
		if err := verifyDigestSignature(entry.Signer.KeyID, digest, entry.Signature); err != nil {
			return "", nil, "", fmt.Errorf("countersignatures[%d]: %w", i, err)
		}
		independent := true
		for _, key := range trustedProducerKeys {
			if strings.EqualFold(hex.EncodeToString(key), entry.Signer.KeyID) {
				independent = false
				break
			}
		}
		report := countersignatureReport{Signer: entry.Signer, Independent: &independent, Checks: entry.Statement.Checks, RecomputedAt: entry.Statement.RecomputedAt}
		if !independent {
			report.State = "not_independent"
			reports = append(reports, report)
			continue
		}
		if directory == nil {
			d, err := fetchCountersignerDirectory(ctx, client, directoryURL)
			if err != nil {
				return "", nil, "", err
			}
			directory = &d
		}
		row, found := resolveSigner(*directory, entry.Signer.KeyID)
		if !found {
			report.State = "unresolved_signer"
			reports = append(reports, report)
			continue
		}
		report.State = "resolved"
		report.SignerName = row.Name
		reports = append(reports, report)
	}
	for _, report := range reports {
		if rank[report.State] > rank[summary] {
			summary = report.State
		}
	}
	return digest, reports, summary, nil
}

func countersignCommands() *cobra.Command {
	group := &cobra.Command{Use: "countersign", Short: "Request and verify third-party countersignatures over an Evidence Bundle digest"}

	request := &cobra.Command{Use: "request", Short: "Request a countersignature and attach the returned entry to the bundle file", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		profile, err := selected(c)
		if err != nil {
			return err
		}
		rawService, _ := c.Flags().GetString("service")
		service, err := validateServiceURL(rawService)
		if err != nil {
			return err
		}
		window, _ := c.Flags().GetString("window")
		if window == "" {
			return inputError("--window is required")
		}
		bundlePath, _ := c.Flags().GetString("bundle")
		outPath, _ := c.Flags().GetString("out")

		var bundle map[string]interface{}
		writePath := outPath
		replace := false
		if bundlePath != "" {
			raw, err := readInput(bundlePath)
			if err != nil {
				return err
			}
			bundle, err = decodeBundleJSON(raw)
			if err != nil {
				return err
			}
			if writePath == "" {
				writePath, replace = bundlePath, true
			}
		} else {
			root, _ := c.Flags().GetString("root")
			if root == "" {
				return inputError("--root is required to build a bundle when --bundle is not given")
			}
			closureDepth, _ := c.Flags().GetInt("closure-depth")
			target, err := openTarget(c.Context(), profile, usePublication)
			if err != nil {
				return err
			}
			bundle, err = AssembleBundle(c.Context(), target.artifacts, target.log, profile.LogID, BundleOptions{Root: root, ClosureDepth: closureDepth, Payloads: "none"})
			if closeErr := target.close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
			if err != nil {
				return err
			}
			if writePath == "" {
				return inputError("--out is required when building a fresh bundle (no existing --bundle file to update)")
			}
		}
		if _, present := bundle["disclosures"]; present {
			return inputError("countersign request refuses a bundle carrying disclosures; only a withheld (payloads none) bundle may be submitted")
		}

		key, err := privateKey(profile.Signing)
		if err != nil {
			return err
		}
		submission, digest, err := buildCountersignSubmission(bundle, window, profile.LogID, key)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 30 * time.Second}
		entries, err := requestCountersignatures(c.Context(), client, service, submission, digest)
		if err != nil {
			return err
		}
		if err := attachCountersignatures(bundle, entries); err != nil {
			return err
		}
		encoded, err := json.Marshal(bundle)
		if err != nil {
			return err
		}
		if err := atomicFile(writePath, encoded, replace); err != nil {
			return err
		}
		return output(c, map[string]any{"bundle_digest": digest, "bundle": writePath, "countersignatures_added": len(entries)})
	}}
	request.Flags().String("service", "", "Countersign service URL (HTTPS; no default)")
	request.Flags().String("window", "", "Attestation window/period label sent to the service")
	request.Flags().String("bundle", "", "Existing Evidence Bundle JSON file to submit (built fresh from --root when omitted)")
	request.Flags().String("out", "", "Where to write the updated bundle (defaults to --bundle, updated in place)")
	request.Flags().String("root", "", "Root Capsule ID (when building a fresh bundle)")
	request.Flags().Int("closure-depth", 2, "Citation closure traversal depth from the root (when building a fresh bundle)")
	group.AddCommand(request)

	verify := &cobra.Command{Use: "verify BUNDLE", Short: "Verify every countersignatures[] entry's signature over the bundle digest and resolve its signer", Args: oneArg, RunE: func(c *cobra.Command, args []string) error {
		profile, err := selected(c)
		if err != nil {
			return err
		}
		raw, err := readInput(args[0])
		if err != nil {
			return err
		}
		bundle, err := decodeBundleJSON(raw)
		if err != nil {
			return err
		}
		rawDirectory, _ := c.Flags().GetString("directory")
		directoryURL, err := validateDirectoryURL(rawDirectory)
		if err != nil {
			return err
		}
		trusted, err := parseKeys(profile.TrustedKeys)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 30 * time.Second}
		digest, reports, summary, err := verifyCountersignatures(c.Context(), client, directoryURL, bundle, trusted)
		if err != nil {
			return err
		}
		if err := output(c, map[string]any{"bundle_digest": digest, "countersignatures": reports, "summary": summary}); err != nil {
			return err
		}
		for _, report := range reports {
			if report.State == "unresolved_signer" {
				return ErrPartial
			}
		}
		return nil
	}}
	verify.Flags().String("directory", "", "Countersigner directory URL (default: "+defaultCountersignerDirectoryURL+")")
	group.AddCommand(verify)

	return group
}
