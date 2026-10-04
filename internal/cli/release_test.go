package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/veraison/go-cose"
)

// A real statement registered at the public witness on 2026-10-04, under a
// test subject, with the registration (and receipt) the witness returned.
func TestReleaseReceiptFromTheLiveWitnessVerifiesOffline(t *testing.T) {
	statement := mustRead(t, "testdata/release/live.release.cose")
	var reg statementRegistration
	require.NoError(t, json.Unmarshal(mustRead(t, "testdata/release/live.release.registration.json"), &reg))
	receipt, err := base64.StdEncoding.DecodeString(reg.ReceiptB64)
	require.NoError(t, err)
	authority, err := hex.DecodeString(dealDefaultWitnessKey)
	require.NoError(t, err)
	require.NoError(t, verifyStatementReceipt(statement, receipt, reg, authority))

	var st releaseStatement
	require.NoError(t, json.Unmarshal(mustRead(t, "testdata/release/live.release.json"), &st))
	require.NoError(t, verifyReleaseStatementSignature(statement, st, "pkg:github/action-state-group/capsule-cli/transparency-test"))

	tampered := append([]byte(nil), statement...)
	tampered[len(tampered)-70] ^= 1 // inside the payload
	assert.Error(t, verifyStatementReceipt(tampered, receipt, reg, authority), "a changed statement is not the one logged")
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	assert.Error(t, verifyStatementReceipt(statement, receipt, reg, other), "nor does the receipt verify under another key")
	moved := reg
	moved.LeafIndex++
	assert.Error(t, verifyStatementReceipt(statement, receipt, moved, authority))
}

// relWitness registers statements as the public witness does (the entry
// hash over the Sig_structure, no check of who signed; a receipt from a
// one-leaf log signed by key) and lists them by subject.
type relWitness struct {
	mu      sync.Mutex
	key     ed25519.PrivateKey
	entries []map[string]any
}

func (w *relWitness) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/transparency/register-statement", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			SignedStatementB64 string `json:"signed_statement_b64"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		statement, err := base64.StdEncoding.DecodeString(req.SignedStatementB64)
		require.NoError(t, err)
		entry, err := statementEntryHash(statement)
		require.NoError(t, err)
		var msg cose.Sign1Message
		require.NoError(t, msg.UnmarshalCBOR(statement))
		w.mu.Lock()
		w.entries = append(w.entries, map[string]any{"entry_hash": hex.EncodeToString(entry), "capsule_id_digest": hex.EncodeToString(msg.Payload)})
		w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{"receipt_b64": base64.StdEncoding.EncodeToString(relMintReceipt(t, entry, w.key)),
			"entry_hash": hex.EncodeToString(entry), "entry_hash_scheme": entryHashSigStructure, "leaf_index": 0, "tree_size": 1})
	})
	mux.HandleFunc("/transparency/statements", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		entries := []map[string]any{}
		if r.URL.Query().Get("subject") == releaseSubject {
			entries = append(entries, w.entries...)
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"entries": entries})
	})
	return mux
}

// relMintReceipt is an RFC 9162 receipt for a one-leaf tree: the root is the
// leaf hash and the path is empty.
func relMintReceipt(t *testing.T, entry []byte, key ed25519.PrivateKey) []byte {
	t.Helper()
	root := sha256.Sum256(append([]byte{0}, entry...))
	proof, err := cbor.Marshal([]any{int64(1), int64(0), [][]byte{}})
	require.NoError(t, err)
	msg := cose.NewSign1Message()
	msg.Headers.Protected.SetAlgorithm(cose.AlgorithmEdDSA)
	msg.Headers.Protected[receiptHeaderVDS] = receiptVDSRFC9162
	msg.Headers.Unprotected[receiptHeaderVDP] = map[any]any{receiptVDPInclusion: []any{proof}}
	msg.Payload = root[:]
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, key)
	require.NoError(t, err)
	require.NoError(t, msg.Sign(rand.Reader, nil, signer))
	msg.Payload = nil
	raw, err := msg.MarshalCBOR()
	require.NoError(t, err)
	return raw
}

// relGitHub serves tags, tag objects, releases and asset downloads.
type relGitHub struct {
	tags     []map[string]any
	refs     map[string]map[string]any
	tagObjs  map[string]map[string]any
	releases []map[string]any
	files    map[string][]byte
}

func (g *relGitHub) handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		last := p[strings.LastIndex(p, "/")+1:]
		write := func(v any, ok bool) {
			if !ok {
				http.NotFound(rw, r)
				return
			}
			_ = json.NewEncoder(rw).Encode(v)
		}
		switch {
		case strings.HasSuffix(p, "/tags") && !strings.Contains(p, "/git/"):
			write(g.tags, true)
		case strings.Contains(p, "/git/ref/tags/"):
			v, ok := g.refs[last]
			write(v, ok)
		case strings.Contains(p, "/git/tags/"):
			v, ok := g.tagObjs[last]
			write(v, ok)
		case strings.HasSuffix(p, "/releases"):
			write(g.releases, true)
		default:
			if b, ok := g.files[p]; ok {
				_, _ = rw.Write(b)
				return
			}
			http.NotFound(rw, r)
		}
	})
}

type relSSHSigner struct{ key, allowed string }

func newRelSSHSigner(t *testing.T, principal string) relSSHSigner {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is needed to sign test tags")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "id")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", principal, "-f", key).Run())
	pub := strings.TrimSpace(string(mustRead(t, key+".pub")))
	allowed := filepath.Join(dir, "allowed_signers")
	require.NoError(t, os.WriteFile(allowed, []byte(principal+` namespaces="git" `+pub+"\n"), 0o600))
	return relSSHSigner{key: key, allowed: allowed}
}

func (s relSSHSigner) sign(t *testing.T, payload string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(file, []byte(payload), 0o600))
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", s.key, "-n", "git", file).Run())
	return string(mustRead(t, file+".sig"))
}

type releaseWorld struct {
	gh      *relGitHub
	wit     *relWitness
	ghURL   string
	witURL  string
	authHex string
	signer  relSSHSigner
	commit  string
}

// The stand-in for gh and Sigstore: a bundle "attests" a release.json when
// it names that file's digest, the release workflow at the tag, and the
// commit.
func fakeAttestation(payload []byte, identity, commit string) []byte {
	sum := sha256.Sum256(payload)
	return []byte(hex.EncodeToString(sum[:]) + " " + identity + " " + commit)
}

func releaseWorkflowIdentity(tag string) string {
	return "https://github.com/" + releaseRepo + "/.github/workflows/release.yml@refs/tags/" + tag
}

func newReleaseWorld(t *testing.T) *releaseWorld {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	w := &releaseWorld{gh: &relGitHub{refs: map[string]map[string]any{}, tagObjs: map[string]map[string]any{}, files: map[string][]byte{}},
		wit: &relWitness{key: key}, authHex: hex.EncodeToString(public),
		signer: newRelSSHSigner(t, "maintainer@example.org"), commit: strings.Repeat("ab", 20)}
	gh := httptest.NewServer(w.gh.handler())
	wit := httptest.NewServer(w.wit.handler(t))
	t.Cleanup(gh.Close)
	t.Cleanup(wit.Close)
	w.ghURL, w.witURL = gh.URL, wit.URL
	saved := releaseAttestationCheck
	releaseAttestationCheck = func(_ context.Context, _ releaseWatchConfig, file, bundle, tag, commit string) error {
		if string(mustRead(t, bundle)) != string(fakeAttestation(mustRead(t, file), releaseWorkflowIdentity(tag), commit)) {
			return errors.New("no attestation by the release workflow at this tag and commit")
		}
		return nil
	}
	t.Cleanup(func() { releaseAttestationCheck = saved })
	return w
}

// addTag adds an annotated tag, signed by signer when it is set.
func (w *releaseWorld) addTag(t *testing.T, tag string, signer *relSSHSigner) {
	t.Helper()
	sha := fmt.Sprintf("%040x", len(w.gh.tags)+1)
	w.gh.tags = append(w.gh.tags, map[string]any{"name": tag, "commit": map[string]any{"sha": w.commit}})
	w.gh.refs[tag] = map[string]any{"object": map[string]any{"type": "tag", "sha": sha}}
	payload := "object " + w.commit + "\ntype commit\ntag " + tag + "\ntagger M <m@example.org> 0 +0000\n\n" + tag + "\n"
	verification := map[string]any{"signature": "", "payload": payload}
	if signer != nil {
		verification["signature"] = signer.sign(t, payload)
	}
	w.gh.tagObjs[sha] = map[string]any{"verification": verification}
}

// addRelease builds a release's files; when register is set it runs
// `release register` against the fake witness and attests release.json as
// release.yml does; then it publishes every file as an asset.
func (w *releaseWorld) addRelease(t *testing.T, tag string, register bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"capsulectl-" + tag + "-linux-amd64", "capsulectl-" + tag + "-darwin-arm64", "capsulectl-" + tag + ".sigstore.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("contents of "+name), 0o600))
	}
	if register {
		out, err := invoke(t, "", "release", "register", "--dir", dir, "--tag", tag, "--commit", w.commit, "--witness", w.witURL, "--witness-key", w.authHex)
		require.NoError(t, err, out)
		payloadName, _, _ := releaseFileNames(tag)
		attestation := fakeAttestation(mustRead(t, filepath.Join(dir, payloadName)), releaseWorkflowIdentity(tag), w.commit)
		require.NoError(t, os.WriteFile(filepath.Join(dir, releaseAttestationName(tag)), attestation, 0o600))
	}
	w.publish(t, tag, dir)
	return dir
}

// publish (re)publishes every file in dir as the release's assets.
func (w *releaseWorld) publish(t *testing.T, tag, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assets := []map[string]any{}
	for _, e := range entries {
		raw := mustRead(t, filepath.Join(dir, e.Name()))
		sum := sha256.Sum256(raw)
		path := "/download/" + tag + "/" + e.Name()
		w.gh.files[path] = raw
		assets = append(assets, map[string]any{"name": e.Name(), "digest": "sha256:" + hex.EncodeToString(sum[:]), "browser_download_url": w.ghURL + path})
	}
	for i, r := range w.gh.releases {
		if r["tag_name"] == tag {
			w.gh.releases[i]["assets"] = assets
			return
		}
	}
	w.gh.releases = append(w.gh.releases, map[string]any{"tag_name": tag, "assets": assets})
}

// register signs payload with a fresh key and registers it at the fake
// witness under our subject, as anyone may.
func (w *releaseWorld) register(t *testing.T, payload []byte, issuer string) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	statement, err := signReleaseStatement(payload, key, issuer, releaseSubject)
	require.NoError(t, err)
	_, _, err = registerStatement(context.Background(), w.witURL, statement)
	require.NoError(t, err)
	return statement
}

func (w *releaseWorld) watch(t *testing.T, extra ...string) (map[string]any, error) {
	t.Helper()
	args := append([]string{"release", "watch", "--github-api", w.ghURL, "--witness", w.witURL, "--witness-key", w.authHex, "--allowed-signers", w.signer.allowed}, extra...)
	out, err := invoke(t, "", args...)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func alarmsOf(result map[string]any) string {
	var lines []string
	for _, a := range result["alarms"].([]any) {
		lines = append(lines, a.(string))
	}
	return strings.Join(lines, "\n")
}

func TestReleaseWatchAcceptsAnIntendedRegisteredRelease(t *testing.T) {
	w := newReleaseWorld(t)
	w.addTag(t, "v1.0.0", &w.signer)
	w.addRelease(t, "v1.0.0", true)
	result, err := w.watch(t)
	require.NoError(t, err, alarmsOf(result))
	assert.Equal(t, "ok", result["state"])
	assert.Equal(t, []any{"v1.0.0"}, result["releases_checked"])
	assert.Equal(t, float64(1), result["statements_in_log"])
}

// replaceStatement re-signs tag's release.json (or payload, when set) with
// a fresh key under issuer, registers it, and puts it on the release.
func (w *releaseWorld) replaceStatement(t *testing.T, tag, issuer string) {
	t.Helper()
	dir := w.addRelease(t, tag, true)
	w.addTag(t, tag, &w.signer)
	payloadName, statementName, _ := releaseFileNames(tag)
	forged := w.register(t, mustRead(t, filepath.Join(dir, payloadName)), issuer)
	require.NoError(t, os.WriteFile(filepath.Join(dir, statementName), forged, 0o600))
	w.publish(t, tag, dir)
}

func TestReleaseWatchAlarms(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, w *releaseWorld)
		want  string
	}{
		"an unsigned tag released (a stolen token or a compromised CI)": {
			setup: func(t *testing.T, w *releaseWorld) { w.addTag(t, "v6.6.6", nil); w.addRelease(t, "v6.6.6", true) },
			want:  "unintended release v6.6.6",
		},
		"a tag signed by a key that is not an allowed signer": {
			setup: func(t *testing.T, w *releaseWorld) {
				rogue := newRelSSHSigner(t, "rogue@example.org")
				w.addTag(t, "v6.6.7", &rogue)
				w.addRelease(t, "v6.6.7", true)
			},
			want: "unintended tag v6.6.7: signed by a key not in the allowed signers",
		},
		"an intended release missing from the log": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.wit.mu.Lock()
				w.wit.entries = nil
				w.wit.mu.Unlock()
			},
			want: "release v1.0.0: its statement is not in the witness's log",
		},
		"a statement anyone registered under our subject, claiming a release": {
			setup: func(t *testing.T, w *releaseWorld) {
				payload, err := jcsOf(releaseStatement{Type: releaseStatementType, Repo: releaseRepo, Tag: "v9.9.9", Commit: strings.Repeat("cd", 20), Artifacts: map[string]string{}})
				require.NoError(t, err)
				w.register(t, payload, releaseIssuer)
			},
			want: "statement under " + releaseSubject + " that does not verify as one of our releases: entry",
		},
		"a statement on an intended release signed by a key its payload does not name": {
			setup: func(t *testing.T, w *releaseWorld) { w.replaceStatement(t, "v1.1.0", releaseIssuer) },
			want:  "release v1.1.0: its statement's signature does not verify: the signature does not verify under its statement key",
		},
		"a statement that names another issuer": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.replaceStatement(t, "v1.1.0", "https://example.org/not-the-release-workflow")
			},
			want: "release v1.1.0: its statement's signature does not verify: issuer https://example.org/not-the-release-workflow",
		},
		"release.json with no attestation": {
			setup: func(t *testing.T, w *releaseWorld) {
				dir := w.addRelease(t, "v1.1.0", true)
				w.addTag(t, "v1.1.0", &w.signer)
				require.NoError(t, os.Remove(filepath.Join(dir, releaseAttestationName("v1.1.0"))))
				w.publish(t, "v1.1.0", dir)
			},
			want: "release v1.1.0: release.json has no attestation",
		},
		"release.json attested by the workflow at another tag": {
			setup: func(t *testing.T, w *releaseWorld) {
				dir := w.addRelease(t, "v1.1.0", true)
				w.addTag(t, "v1.1.0", &w.signer)
				payloadName, _, _ := releaseFileNames("v1.1.0")
				other := fakeAttestation(mustRead(t, filepath.Join(dir, payloadName)), releaseWorkflowIdentity("v1.0.0"), w.commit)
				require.NoError(t, os.WriteFile(filepath.Join(dir, releaseAttestationName("v1.1.0")), other, 0o600))
				w.publish(t, "v1.1.0", dir)
			},
			want: "release v1.1.0: release.json's attestation does not verify as the release workflow at refs/tags/v1.1.0",
		},
		"an asset that is not what the statement records": {
			setup: func(t *testing.T, w *releaseWorld) {
				for _, a := range w.gh.releases[0]["assets"].([]map[string]any) {
					if a["name"] == "capsulectl-v1.0.0-linux-amd64" {
						a["digest"] = "sha256:" + strings.Repeat("00", 32)
					}
				}
			},
			want: "release v1.0.0: asset capsulectl-v1.0.0-linux-amd64 (sha256 " + strings.Repeat("00", 32) + ") is not what its statement records",
		},
		"an intended release with no statement": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.addTag(t, "v1.1.0", &w.signer)
				w.addRelease(t, "v1.1.0", false)
			},
			want: "release v1.1.0 has no release statement",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newReleaseWorld(t)
			w.addTag(t, "v1.0.0", &w.signer)
			w.addRelease(t, "v1.0.0", true)
			c.setup(t, w)
			result, err := w.watch(t)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Equal(t, "alarm", result["state"])
			assert.Contains(t, alarmsOf(result), c.want)
		})
	}
}

// Tags made before signing began are accepted as named exceptions, and a
// release made then needs no statement.
func TestReleaseWatchKnownExceptions(t *testing.T) {
	w := newReleaseWorld(t)
	w.addTag(t, "v0.1.0-rc4", nil)
	w.addRelease(t, "v0.1.0-rc4", false)
	result, err := w.watch(t)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Contains(t, alarmsOf(result), "unintended release v0.1.0-rc4")
	result, err = w.watch(t, "--known-unsigned", "v0.1.0-rc4")
	require.NoError(t, err, alarmsOf(result))
	assert.Equal(t, "ok", result["state"])
}

func TestReleaseWatchNeedsAllowedSigners(t *testing.T) {
	_, err := invoke(t, "", "release", "watch")
	require.ErrorIs(t, err, ErrInput)
}

func TestReleaseRegisterNeedsTheTaggedCommit(t *testing.T) {
	_, err := invoke(t, "", "release", "register", "--dir", t.TempDir(), "--tag", "v1.0.0", "--commit", "abc")
	require.ErrorIs(t, err, ErrInput)
}
