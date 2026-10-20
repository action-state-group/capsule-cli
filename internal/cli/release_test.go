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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	authority, err := hex.DecodeString(DealDefaultWitnessKey)
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
	msg.Headers.Protected[int64(395)] = int64(1)                               // vds: RFC9162_SHA256
	msg.Headers.Unprotected[int64(396)] = map[any]any{int64(-1): []any{proof}} // vdp: inclusion proofs
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
	pageSize int // when set, lists are served in pages linked by rel="next"
	tags     []map[string]any
	refs     map[string]map[string]any
	tagObjs  map[string]map[string]any
	commits  map[string]map[string]any
	runs     map[string]map[string]any   // run id -> the run (actor, triggering_actor)
	reviews  map[string][]map[string]any // run id -> its approvals
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
		list := func(items []map[string]any) {
			if g.pageSize == 0 {
				write(items, true)
				return
			}
			page := 0
			_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
			start := page * g.pageSize
			end := start + g.pageSize
			if start > len(items) {
				start = len(items)
			}
			if end >= len(items) {
				end = len(items)
			} else {
				next := *r.URL
				q := next.Query()
				q.Set("page", fmt.Sprint(page+1))
				next.RawQuery = q.Encode()
				rw.Header().Set("Link", `<http://`+r.Host+next.String()+`>; rel="next", <http://`+r.Host+`/last>; rel="last"`)
			}
			write(items[start:end], true)
		}
		switch {
		case strings.HasSuffix(p, "/tags") && !strings.Contains(p, "/git/"):
			list(g.tags)
		case strings.Contains(p, "/git/ref/tags/"):
			v, ok := g.refs[last]
			write(v, ok)
		case strings.Contains(p, "/git/tags/"):
			v, ok := g.tagObjs[last]
			write(v, ok)
		case strings.Contains(p, "/git/commits/"):
			v, ok := g.commits[last]
			write(v, ok)
		case strings.Contains(p, "/actions/runs/") && strings.HasSuffix(p, "/approvals"):
			run := strings.TrimSuffix(strings.TrimPrefix(p[strings.Index(p, "/actions/runs/"):], "/actions/runs/"), "/approvals")
			v, ok := g.reviews[run]
			write(v, ok)
		case strings.Contains(p, "/actions/runs/"):
			v, ok := g.runs[last]
			write(v, ok)
		case strings.HasSuffix(p, "/releases"):
			list(g.releases)
		default:
			if b, ok := g.files[p]; ok {
				_, _ = rw.Write(b)
				return
			}
			http.NotFound(rw, r)
		}
	})
}

type releaseWorld struct {
	gh      *relGitHub
	wit     *relWitness
	ghURL   string
	witURL  string
	authHex string
	commit  string
}

// The stand-in for gh and Sigstore: a bundle "attests" a release.json when
// it names that file's digest, the release workflow at the tag, and the
// commit. Like a real bundle, its DSSE payload names the workflow run.
func fakeAttestation(payload []byte, identity, commit, run string) []byte {
	sum := sha256.Sum256(payload)
	statement, _ := json.Marshal(map[string]any{
		"fake": hex.EncodeToString(sum[:]) + " " + identity + " " + commit,
		"predicate": map[string]any{"runDetails": map[string]any{"metadata": map[string]any{
			"invocationId": "https://github.com/" + releaseRepo + "/actions/runs/" + run + "/attempts/1"}}},
	})
	bundle, _ := json.Marshal(map[string]any{"dsseEnvelope": map[string]any{"payload": statement}})
	return bundle
}

// fakeAttested reports whether bundle is fakeAttestation's for payload,
// identity and commit, whatever run it names.
func fakeAttested(bundle, payload []byte, identity, commit string) bool {
	var b struct {
		DSSE struct {
			Payload []byte `json:"payload"`
		} `json:"dsseEnvelope"`
	}
	var st struct {
		Fake string `json:"fake"`
	}
	sum := sha256.Sum256(payload)
	return json.Unmarshal(bundle, &b) == nil && json.Unmarshal(b.DSSE.Payload, &st) == nil &&
		st.Fake == hex.EncodeToString(sum[:])+" "+identity+" "+commit
}

func releaseWorkflowIdentity(tag string) string {
	return "https://github.com/" + releaseRepo + "/.github/workflows/release.yml@refs/tags/" + tag
}

func newReleaseWorld(t *testing.T) *releaseWorld {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	w := &releaseWorld{gh: &relGitHub{refs: map[string]map[string]any{}, tagObjs: map[string]map[string]any{}, commits: map[string]map[string]any{},
		runs: map[string]map[string]any{}, reviews: map[string][]map[string]any{}, files: map[string][]byte{}},
		wit: &relWitness{key: key}, authHex: hex.EncodeToString(public), commit: strings.Repeat("ab", 20)}
	gh := httptest.NewServer(w.gh.handler())
	wit := httptest.NewServer(w.wit.handler(t))
	t.Cleanup(gh.Close)
	t.Cleanup(wit.Close)
	w.ghURL, w.witURL = gh.URL, wit.URL
	saved := releaseAttestationCheck
	releaseAttestationCheck = func(_ context.Context, _ releaseWatchConfig, file, bundle, tag, commit string) error {
		if !fakeAttested(mustRead(t, bundle), mustRead(t, file), releaseWorkflowIdentity(tag), commit) {
			return errors.New("no attestation by the release workflow at this tag and commit")
		}
		return nil
	}
	t.Cleanup(func() { releaseAttestationCheck = saved })
	return w
}

// addTag adds an annotated tag.
func (w *releaseWorld) addTag(t *testing.T, tag string) {
	t.Helper()
	w.addTagAt(t, tag, time.Unix(0, 0))
}

// addTagAt adds an annotated tag whose tagger time is at.
func (w *releaseWorld) addTagAt(t *testing.T, tag string, at time.Time) {
	t.Helper()
	sha := fmt.Sprintf("%040x", len(w.gh.tags)+1)
	w.gh.tags = append(w.gh.tags, map[string]any{"name": tag, "commit": map[string]any{"sha": w.commit}})
	w.gh.refs[tag] = map[string]any{"object": map[string]any{"type": "tag", "sha": sha}}
	w.gh.tagObjs[sha] = map[string]any{"tag": tag, "tagger": map[string]any{"name": "M", "date": at.UTC().Format(time.RFC3339)}}
}

// addLightweightTagAt adds a lightweight tag on a commit made at at.
func (w *releaseWorld) addLightweightTagAt(t *testing.T, tag string, at time.Time) {
	t.Helper()
	w.gh.tags = append(w.gh.tags, map[string]any{"name": tag, "commit": map[string]any{"sha": w.commit}})
	w.gh.refs[tag] = map[string]any{"object": map[string]any{"type": "commit", "sha": w.commit}}
	w.gh.commits[w.commit] = map[string]any{"committer": map[string]any{"name": "M", "date": at.UTC().Format(time.RFC3339)}}
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
		run := w.addRun(tag, "pusher", "pusher", "reviewer")
		attestation := fakeAttestation(mustRead(t, filepath.Join(dir, payloadName)), releaseWorkflowIdentity(tag), w.commit, run)
		require.NoError(t, os.WriteFile(filepath.Join(dir, releaseAttestationName(tag)), attestation, 0o600))
	}
	w.publish(t, tag, dir)
	return dir
}

// addRun records a release run for tag, pushed by actor and started by
// triggering, approved by approver in the environment the tag implies (none
// when approver is empty), and returns its id.
func (w *releaseWorld) addRun(tag, actor, triggering, approver string) string {
	run := fmt.Sprint(1000 + len(w.gh.runs))
	w.gh.runs[run] = map[string]any{"actor": map[string]any{"login": actor}, "triggering_actor": map[string]any{"login": triggering}}
	w.gh.reviews[run] = []map[string]any{}
	if approver != "" {
		w.approve(run, releaseEnvironment(tag), approver)
	}
	return run
}

// approve adds an approval of run in env by login.
func (w *releaseWorld) approve(run, env, login string) {
	w.gh.reviews[run] = append(w.gh.reviews[run], map[string]any{"state": "approved",
		"environments": []map[string]any{{"name": env}}, "user": map[string]any{"login": login}})
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
	args := append([]string{"release", "watch", "--github-api", w.ghURL, "--witness", w.witURL, "--witness-key", w.authHex}, extra...)
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
	w.addTag(t, "v1.0.0")
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
	w.addTag(t, tag)
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
		"a release whose tag was deleted": {
			setup: func(t *testing.T, w *releaseWorld) { w.addRelease(t, "v6.6.6", true) },
			want:  "release v6.6.6 has no v* tag of that name",
		},
		"a tag moved to another commit after its release": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.gh.tags[0]["commit"] = map[string]any{"sha": strings.Repeat("cd", 20)}
			},
			want: "release v1.0.0: its statement names " + releaseRepo + " v1.0.0 at " + strings.Repeat("ab", 20) + ", not this tag's commit " + strings.Repeat("cd", 20),
		},
		"a release missing from the log": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.wit.mu.Lock()
				w.wit.entries = nil
				w.wit.mu.Unlock()
			},
			want: "release v1.0.0: its statement is not in the witness's log",
		},
		"a statement anyone registered under our subject, claiming a release": {
			setup: func(t *testing.T, w *releaseWorld) {
				payload, err := JCSOf(releaseStatement{Type: releaseStatementType, Repo: releaseRepo, Tag: "v9.9.9", Commit: strings.Repeat("cd", 20), Artifacts: map[string]string{}})
				require.NoError(t, err)
				w.register(t, payload, releaseIssuer)
			},
			want: "statement under " + releaseSubject + " that does not verify as one of our releases: entry",
		},
		"a statement on a release signed by a key its payload does not name": {
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
				w.addTag(t, "v1.1.0")
				require.NoError(t, os.Remove(filepath.Join(dir, releaseAttestationName("v1.1.0"))))
				w.publish(t, "v1.1.0", dir)
			},
			want: "release v1.1.0: release.json has no attestation",
		},
		"release.json attested by the workflow at another tag": {
			setup: func(t *testing.T, w *releaseWorld) {
				dir := w.addRelease(t, "v1.1.0", true)
				w.addTag(t, "v1.1.0")
				payloadName, _, _ := releaseFileNames("v1.1.0")
				other := fakeAttestation(mustRead(t, filepath.Join(dir, payloadName)), releaseWorkflowIdentity("v1.0.0"), w.commit, "1")
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
		"a run with no approval (a release.yml with no environment)": {
			setup: func(t *testing.T, w *releaseWorld) { w.gh.reviews["1000"] = nil },
			want:  "release v1.0.0: run 1000 has no approval in the release environment",
		},
		"a full release approved only in the pre-release environment": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.gh.reviews["1000"] = nil
				w.approve("1000", "release-candidate", "reviewer")
			},
			want: "release v1.0.0: run 1000 has no approval in the release environment",
		},
		"a rejected run": {
			setup: func(t *testing.T, w *releaseWorld) { w.gh.reviews["1000"][0]["state"] = "rejected" },
			want:  "release v1.0.0: run 1000 has no approval in the release environment",
		},
		"a full release approved by whoever pushed its tag": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.gh.reviews["1000"] = nil
				w.approve("1000", "release", "pusher")
			},
			want: "release v1.0.0: run 1000 was approved in release only by pusher, who pushed the tag or started the run",
		},
		"a full release re-run and approved by whoever re-ran it": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.gh.runs["1000"]["triggering_actor"] = map[string]any{"login": "reviewer"}
			},
			want: "release v1.0.0: run 1000 was approved in release only by reviewer, who pushed the tag or started the run",
		},
		"an attestation that names another repository's run": {
			setup: func(t *testing.T, w *releaseWorld) {
				dir := w.addRelease(t, "v1.1.0", true)
				w.addTag(t, "v1.1.0")
				file := filepath.Join(dir, releaseAttestationName("v1.1.0"))
				var b map[string]map[string][]byte
				require.NoError(t, json.Unmarshal(mustRead(t, file), &b))
				b["dsseEnvelope"]["payload"] = []byte(strings.Replace(string(b["dsseEnvelope"]["payload"]), releaseRepo+"/actions", "someone/else/actions", 1))
				bundle, err := json.Marshal(b)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(file, bundle, 0o600))
				w.publish(t, "v1.1.0", dir)
			},
			want: "release v1.1.0: release.json's attestation names no workflow run of " + releaseRepo,
		},
		"a release with no statement": {
			setup: func(t *testing.T, w *releaseWorld) {
				w.addTag(t, "v1.1.0")
				w.addRelease(t, "v1.1.0", false)
			},
			want: "release v1.1.0 has no release statement",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newReleaseWorld(t)
			w.addTag(t, "v1.0.0")
			w.addRelease(t, "v1.0.0", true)
			c.setup(t, w)
			result, err := w.watch(t)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Equal(t, "alarm", result["state"])
			assert.Contains(t, alarmsOf(result), c.want)
		})
	}
}

// The releases cut before `release register` existed need no statement;
// any later release does.
func TestReleaseWatchReleasesBeforeStatements(t *testing.T) {
	w := newReleaseWorld(t)
	for _, tag := range []string{"v0.1.0-rc1", "v0.1.0-rc4"} {
		w.addTag(t, tag)
		w.addRelease(t, tag, false)
	}
	result, err := w.watch(t)
	require.NoError(t, err, alarmsOf(result))
	assert.Equal(t, "ok", result["state"])
	assert.Equal(t, []any{"v0.1.0-rc1", "v0.1.0-rc4"}, result["tags"])

	w.addTag(t, "v0.1.0-rc5")
	w.addRelease(t, "v0.1.0-rc5", false)
	result, err = w.watch(t)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Equal(t, "release v0.1.0-rc5 has no release statement", alarmsOf(result))
}

// A pre-release may be approved by whoever pushed its tag. The releases cut
// before release.yml waited for approval (up to v0.1.0-rc17) need none; any
// later one does.
func TestReleaseWatchApprovals(t *testing.T) {
	w := newReleaseWorld(t)
	w.addTag(t, "v1.1.0-rc1")
	w.addRelease(t, "v1.1.0-rc1", true)
	run := fmt.Sprint(1000 + len(w.gh.runs) - 1)
	w.gh.reviews[run] = nil
	w.approve(run, "release-candidate", "pusher")
	result, err := w.watch(t)
	require.NoError(t, err, alarmsOf(result))

	w.addTag(t, "v0.1.0-rc17")
	w.addRelease(t, "v0.1.0-rc17", true)
	w.gh.reviews[fmt.Sprint(1000+len(w.gh.runs)-1)] = nil
	result, err = w.watch(t)
	require.NoError(t, err, alarmsOf(result))

	w.addTag(t, "v0.1.0-rc18")
	w.addRelease(t, "v0.1.0-rc18", true)
	run = fmt.Sprint(1000 + len(w.gh.runs) - 1)
	w.gh.reviews[run] = nil
	result, err = w.watch(t)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Equal(t, "release v0.1.0-rc18: run "+run+" has no approval in the release-candidate environment", alarmsOf(result))
}

// The signed-tag flags are gone: the monitor needs no key file.
func TestReleaseWatchHasNoSignerFlags(t *testing.T) {
	for _, flag := range []string{"--allowed-signers", "--known-unsigned", "--known-unsigned-file"} {
		_, err := invoke(t, "", "release", "watch", flag, "x")
		require.Error(t, err, flag)
		assert.ErrorContains(t, err, "has no flag "+flag)
	}
}

func TestReleaseRegisterNeedsTheTaggedCommit(t *testing.T) {
	_, err := invoke(t, "", "release", "register", "--dir", t.TempDir(), "--tag", "v1.0.0", "--commit", "abc")
	require.ErrorIs(t, err, ErrInput)
}

// A release tag is a promise of a release: past the grace window (time for
// a reviewer to approve the run, and for the build), a tag with no release
// (never approved, failed, or deleted) is an alarm.
func TestReleaseWatchTagWithNoRelease(t *testing.T) {
	saved := releaseNow
	t.Cleanup(func() { releaseNow = saved })
	tagged := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	for name, add := range map[string]func(w *releaseWorld){
		"annotated":   func(w *releaseWorld) { w.addTagAt(t, "v1.1.0", tagged) },
		"lightweight": func(w *releaseWorld) { w.addLightweightTagAt(t, "v1.1.0", tagged) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newReleaseWorld(t)
			w.addTag(t, "v1.0.0")
			w.addRelease(t, "v1.0.0", true)
			add(w)

			releaseNow = func() time.Time { return tagged.Add(20 * time.Hour) }
			result, err := w.watch(t)
			require.NoError(t, err, "within the grace window the release may still await approval: %s", alarmsOf(result))

			releaseNow = func() time.Time { return tagged.Add(25 * time.Hour) }
			result, err = w.watch(t)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Equal(t, "tag v1.1.0 has no release (tagged 2026-10-04T12:00:00Z; grace 24h0m0s)", alarmsOf(result))

			result, err = w.watch(t, "--release-grace", "48h")
			require.NoError(t, err, alarmsOf(result))

			// A tag dated in the future cannot hold the alarm off.
			releaseNow = func() time.Time { return tagged.Add(-2 * time.Hour) }
			result, err = w.watch(t)
			require.ErrorIs(t, err, ErrAlarm)
			assert.Equal(t, "tag v1.1.0 has no release and is dated in the future (2026-10-04T12:00:00Z)", alarmsOf(result))
			releaseNow = func() time.Time { return tagged.Add(-30 * time.Minute) }
			result, err = w.watch(t)
			require.NoError(t, err, "within the clock skew: %s", alarmsOf(result))
		})
	}
}

// Tags and releases are read across every page; past the page limit the
// monitor refuses rather than check a partial list.
func TestReleaseWatchFollowsEveryPage(t *testing.T) {
	w := newReleaseWorld(t)
	w.gh.pageSize = 1
	for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		w.addTag(t, tag)
		w.addRelease(t, tag, true)
	}
	result, err := w.watch(t)
	require.NoError(t, err, alarmsOf(result))
	assert.Equal(t, []any{"v1.0.0", "v1.1.0", "v1.2.0"}, result["releases_checked"])

	// A release with no tag, on the last page, is still seen.
	w.addRelease(t, "v6.6.6", true)
	result, err = w.watch(t)
	require.ErrorIs(t, err, ErrAlarm)
	assert.Contains(t, alarmsOf(result), "release v6.6.6 has no v* tag of that name")

	saved := releaseMaxPages
	t.Cleanup(func() { releaseMaxPages = saved })
	releaseMaxPages = 2
	_, err = invoke(t, "", "release", "watch", "--github-api", w.ghURL, "--witness", w.witURL, "--witness-key", w.authHex)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrAlarm, "a partial list is a failure to check, not a verdict")
}

// The witness answers in one response; if it ever pages, or the answer is
// cut off, the monitor fails closed.
func TestReleaseWatchFailsClosedOnAPartialLog(t *testing.T) {
	paged := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(rw).Encode(map[string]any{"entries": []any{}, "next_cursor": "abc"})
	}))
	t.Cleanup(paged.Close)
	linked := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Link", `<`+"http://"+r.Host+`/more>; rel="next"`)
		_ = json.NewEncoder(rw).Encode(map[string]any{"entries": []any{}})
	}))
	t.Cleanup(linked.Close)

	for name, witness := range map[string]string{"a cursor": paged.URL, "a Link header": linked.URL} {
		t.Run(name, func(t *testing.T) {
			w := newReleaseWorld(t)
			_, err := invoke(t, "", "release", "watch", "--github-api", w.ghURL, "--witness", witness, "--witness-key", w.authHex)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrAlarm)
		})
	}
	t.Run("an answer over the size limit", func(t *testing.T) {
		w := newReleaseWorld(t)
		w.addTag(t, "v1.0.0")
		w.addRelease(t, "v1.0.0", true)
		saved := releaseMaxListBody
		t.Cleanup(func() { releaseMaxListBody = saved })
		releaseMaxListBody = 64
		_, err := invoke(t, "", "release", "watch", "--github-api", w.ghURL, "--witness", w.witURL, "--witness-key", w.authHex)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrAlarm)
		assert.ErrorContains(t, err, "refusing to check part of it", "refused for its size, not because a cut-off answer happened not to parse")
	})
}

func TestNextLink(t *testing.T) {
	assert.Equal(t, "https://api.github.com/x?page=2", nextLink(`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`))
	assert.Equal(t, "https://api.github.com/x?page=2", nextLink(`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=2>; rel="next"`))
	assert.Equal(t, "", nextLink(`<https://api.github.com/x?page=1>; rel="prev"`))
	assert.Equal(t, "", nextLink(""))
}

// The run is read from a real attestation bundle: v0.1.0-rc17's, as
// published with that release.
func TestReleaseApprovalReadsTheRunFromARealBundle(t *testing.T) {
	w := newReleaseWorld(t)
	w.gh.runs["39489783877"] = map[string]any{"actor": map[string]any{"login": "StevenMih"}, "triggering_actor": map[string]any{"login": "StevenMih"}}
	w.gh.reviews["39489783877"] = nil
	cfg := releaseWatchConfig{repo: releaseRepo, api: w.ghURL}
	bundle := mustRead(t, "testdata/release/v0.1.0-rc17.release.sigstore.json")
	why, err := cfg.checkApproval(context.Background(), bundle, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "run 39489783877 has no approval in the release environment", why)

	w.approve("39489783877", "release", "ethanyzhang")
	why, err = cfg.checkApproval(context.Background(), bundle, "v1.0.0")
	require.NoError(t, err)
	assert.Empty(t, why)
}
