package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The answer vectors under testdata/evidence-request were produced by the
// Python responder (capsule_emit.evidence_request.answer, capsule-emit 0.8.2);
// see the README there. Reading them here is the cross-implementation check.
func evidenceVector(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", "evidence-request", name))
	require.NoError(t, e)
	return b
}

// fakeNode serves the plugin tool route. answer maps the request it receives
// to the bytes it returns; seen records every call.
type nodeCall struct {
	Auth    string
	PeerID  string
	Request json.RawMessage
}

func fakeNode(t *testing.T, answer func(request []byte) (int, []byte)) (*httptest.Server, *[]nodeCall) {
	t.Helper()
	var seen []nodeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != meshEvidenceRequestPath {
			http.NotFound(w, r)
			return
		}
		body, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		var args struct {
			PeerID  string          `json:"peer_id"`
			Request json.RawMessage `json:"request"`
		}
		require.NoError(t, json.Unmarshal(body, &args))
		seen = append(seen, nodeCall{Auth: r.Header.Get("Authorization"), PeerID: args.PeerID, Request: args.Request})
		status, out := answer(args.Request)
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// serveVectors answers each subject kind with its Python-produced vector.
func serveVectors(t *testing.T) func([]byte) (int, []byte) {
	return func(request []byte) (int, []byte) {
		var r struct {
			Subject struct {
				Kind      string `json:"kind"`
				CapsuleID string `json:"capsule_id"`
			} `json:"subject"`
		}
		require.NoError(t, json.Unmarshal(request, &r))
		switch {
		case r.Subject.Kind == "record" && r.Subject.CapsuleID == strings.Repeat("0", 64):
			return http.StatusOK, evidenceVector(t, "no_such_record.answer.json")
		case r.Subject.Kind == "record":
			return http.StatusOK, evidenceVector(t, "record.answer.json")
		case r.Subject.Kind == "range":
			return http.StatusOK, evidenceVector(t, "range.answer.json")
		default:
			return http.StatusOK, evidenceVector(t, "chain_segment.answer.json")
		}
	}
}

func meshProfile(t *testing.T, url string, extra ...string) {
	t.Helper()
	args := append([]string{"profile", "create", "--name", "node", "--type", "mesh-plugin", "--url", url}, extra...)
	_, e := invoke(t, "", args...)
	require.NoError(t, e)
}

func vectorCapsuleID(t *testing.T, name string) string {
	var r struct {
		Subject struct {
			CapsuleID string `json:"capsule_id"`
		} `json:"subject"`
	}
	doc := evidenceVector(t, name)
	require.NoError(t, json.Unmarshal(doc, &r))
	return r.Subject.CapsuleID
}

func TestMeshPluginProfileValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("node-token-value\n"), 0o600))
	for _, bad := range [][]string{
		{}, // no url
		{"--url", "ftp://127.0.0.1:1"},
		{"--url", "http://127.0.0.1:1/api"},
		{"--url", "http://user:pw@127.0.0.1:1"},
		{"--url", "http://127.0.0.1:1?x=1"},
		{"--url", "http://node.example:3131", "--token-file", tokenFile}, // token over cleartext to a non-loopback host
	} {
		args := append([]string{"profile", "create", "--name", "bad", "--type", "mesh-plugin"}, bad...)
		_, e := invoke(t, "", args...)
		require.ErrorIs(t, e, ErrInput, "%v", bad)
	}
	for i, good := range [][]string{
		{"--url", "http://127.0.0.1:3131", "--token-file", tokenFile},
		{"--url", "http://[::1]:3131/", "--token-file", tokenFile},
		{"--url", "http://node.example:3131"},
		{"--url", "https://node.example", "--token-file", tokenFile},
	} {
		args := append([]string{"profile", "create", "--name", "good" + string(rune('a'+i)), "--type", "mesh-plugin"}, good...)
		_, e := invoke(t, "", args...)
		require.NoError(t, e, "%v", good)
	}
	_, e := invoke(t, "", "profile", "create", "--name", "literal", "--type", "mesh-plugin", "--url", "http://127.0.0.1:3131", "--token", "literal-token-value")
	require.NoError(t, e)
	out, e := invoke(t, "", "profile", "show", "literal")
	require.NoError(t, e)
	assert.NotContains(t, out, "literal-token-value")
	assert.Contains(t, out, "[redacted]")
}

func TestBookVerbsAgainstPythonResponderVectors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv, seen := fakeNode(t, serveVectors(t))
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("node-token-value\n"), 0o600))
	meshProfile(t, srv.URL, "--token-file", tokenFile)

	// The request the verb builds is byte-identical to the one the Python
	// responder digested for the same subject.
	id := vectorCapsuleID(t, "record.request.json")
	out, e := invoke(t, "", "book", "get", "--profile", "node", "--party", "peer-a", "--capsule-id", id)
	require.NoError(t, e)
	var got evidenceAnswer
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	pythonRecordRequest := evidenceVector(t, "record.request.json")
	want := evidenceAnswer{
		RequestDigest: sha256Hex(string(pythonRecordRequest)),
		Answer:        "artifact",
		SubjectKind:   "record",
		CapsuleIDs:    []string{id},
		Response:      compactJSON(t, evidenceVector(t, "record.answer.json")),
	}
	assert.Equal(t, want, got)
	wantCalls := []nodeCall{{Auth: "Bearer node-token-value", PeerID: "peer-a", Request: json.RawMessage(pythonRecordRequest)}}
	assert.Equal(t, wantCalls, *seen)
	assert.NotContains(t, out, "node-token-value")

	out, e = invoke(t, "", "book", "list", "--profile", "node", "--party", "peer-a", "--selector", "a..b", "--page-size", "2")
	require.NoError(t, e)
	got = evidenceAnswer{}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	rangeVector := evidenceVector(t, "range.answer.json")
	var ranged struct {
		Bundles []struct {
			CapsuleID string `json:"capsule_id"`
		} `json:"bundles"`
	}
	require.NoError(t, json.Unmarshal(rangeVector, &ranged))
	require.Len(t, ranged.Bundles, 2)
	sentRange := `{"page":{"size":2},"subject":{"kind":"range","selector":"a..b"}}`
	want = evidenceAnswer{
		RequestDigest: sha256Hex(sentRange),
		Answer:        "artifact",
		SubjectKind:   "range",
		CapsuleIDs:    []string{ranged.Bundles[0].CapsuleID, ranged.Bundles[1].CapsuleID},
		NextPageToken: "2",
		Response:      compactJSON(t, rangeVector),
	}
	assert.Equal(t, want, got)
	assert.Equal(t, sentRange, string((*seen)[1].Request))

	out, e = invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
	require.NoError(t, e)
	var head struct {
		Head chainHead `json:"head"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &head))
	chain := evidenceVector(t, "chain_segment.answer.json")
	link := chainVectorLink(t, chain)
	cp := link.Checkpoint
	wantHead := chainHead{LogID: cp.LogID, MMRSize: cp.MMRSize, Root: cp.Root, KeyID: cp.KeyID, Timestamp: cp.Timestamp}
	assert.Equal(t, wantHead, head.Head)
	pythonChainRequest := evidenceVector(t, "chain_segment.request.json")
	assert.Equal(t, string(pythonChainRequest), string((*seen)[2].Request))

	out, e = invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a", "--responder-checkpoint-key", strings.ToUpper(cp.KeyID))
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal([]byte(out), &head))
	wantHead.Pinned = true
	assert.Equal(t, wantHead, head.Head)

	// A request file in any key order and spacing is sent in the canonical
	// form the Python responder digested, so its signed refusal binds to it.
	pythonRefusalRequest := evidenceVector(t, "no_such_record.request.json")
	reordered := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(reordered, []byte("{\n  \"subject\": {\n    \"capsule_id\": \""+strings.Repeat("0", 64)+"\",\n    \"kind\": \"record\"\n  }\n}\n"), 0o600))
	refusalVector := evidenceVector(t, "no_such_record.answer.json")
	var refusal refusalWire
	require.NoError(t, json.Unmarshal(refusalVector, &refusal))
	for _, pinned := range []bool{false, true} {
		args := []string{"book", "request", "--profile", "node", "--party", "peer-a", "--request", reordered}
		if pinned {
			args = append(args, "--responder-key", refusal.KeyID)
		}
		out, e = invoke(t, "", args...)
		require.NoError(t, e)
		got = evidenceAnswer{}
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		want = evidenceAnswer{
			RequestDigest: sha256Hex(string(pythonRefusalRequest)),
			Answer:        "refusal",
			Reason:        "no_such_record",
			Signer:        refusal.KeyID,
			SignerPinned:  pinned,
			Response:      compactJSON(t, refusalVector),
		}
		assert.Equal(t, want, got)
		lastSent := (*seen)[len(*seen)-1].Request
		assert.Equal(t, string(pythonRefusalRequest), string(lastSent))
	}
}

func TestBookVerbsRefuseWhatTheyCannotStandBehind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var serve func([]byte) (int, []byte)
	srv, _ := fakeNode(t, func(r []byte) (int, []byte) { return serve(r) })
	meshProfile(t, srv.URL)

	t.Run("get answered with another record", func(t *testing.T) {
		serve = serveVectors(t)
		_, e := invoke(t, "", "book", "get", "--profile", "node", "--party", "peer-a", "--capsule-id", strings.Repeat("e", 64))
		require.ErrorIs(t, e, ErrConflict)
	})
	refusalRequest := filepath.Join("testdata", "evidence-request", "no_such_record.request.json")
	t.Run("refusal replayed for a different request", func(t *testing.T) {
		serve = func([]byte) (int, []byte) { return http.StatusOK, evidenceVector(t, "no_such_record.answer.json") }
		_, e := invoke(t, "", "book", "get", "--profile", "node", "--party", "peer-a", "--capsule-id", strings.Repeat("1", 64))
		require.ErrorIs(t, e, ErrConflict)
	})
	t.Run("refusal signed by a key other than the responder's", func(t *testing.T) {
		serve = serveVectors(t)
		_, e := invoke(t, "", "book", "request", "--profile", "node", "--party", "peer-a", "--request", refusalRequest, "--responder-key", strings.Repeat("ab", 32))
		require.ErrorIs(t, e, ErrConflict)
	})
	t.Run("artifact for a different subject kind", func(t *testing.T) {
		serve = func([]byte) (int, []byte) { return http.StatusOK, evidenceVector(t, "record.answer.json") }
		_, e := invoke(t, "", "book", "list", "--profile", "node", "--party", "peer-a", "--selector", "a..b")
		require.ErrorIs(t, e, ErrConflict)
	})
	t.Run("answer that is both an artifact and a refusal", func(t *testing.T) {
		both := replaceOnce(t, evidenceVector(t, "no_such_record.answer.json"), `"reason":`, `"bundles": [{}], "subject_kind": "record", "v": 1, "reason":`)
		serve = func([]byte) (int, []byte) { return http.StatusOK, both }
		_, e := invoke(t, "", "book", "request", "--profile", "node", "--party", "peer-a", "--request", refusalRequest)
		require.ErrorIs(t, e, ErrPartial)
	})
	t.Run("refusal with a broken signature", func(t *testing.T) {
		refusal := evidenceVector(t, "no_such_record.answer.json")
		var r refusalWire
		require.NoError(t, json.Unmarshal(refusal, &r))
		tampered := replaceOnce(t, refusal, `"sig": "`+r.Sig+`"`, `"sig": "`+flipLastHexByte(t, r.Sig)+`"`)
		serve = func([]byte) (int, []byte) { return http.StatusOK, tampered }
		_, e := invoke(t, "", "book", "request", "--profile", "node", "--party", "peer-a", "--request", refusalRequest)
		require.ErrorIs(t, e, ErrPartial)
	})
	t.Run("head signed by a key other than the pinned one", func(t *testing.T) {
		serve = serveVectors(t)
		_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a", "--responder-checkpoint-key", strings.Repeat("ab", 32))
		require.ErrorIs(t, e, ErrConflict)
	})
	chain := evidenceVector(t, "chain_segment.answer.json")
	cp := chainVectorLink(t, chain).Checkpoint
	for _, tamper := range []struct{ field, old, replacement string }{
		{"log_id", `"log_id": "` + cp.LogID + `"`, `"log_id": "` + strings.Repeat("1", 64) + `"`},
		{"mmr_size", fmt.Sprintf(`"mmr_size": %d`, cp.MMRSize), fmt.Sprintf(`"mmr_size": %d`, cp.MMRSize+36)},
		{"root", `"root": "` + cp.Root + `"`, `"root": "` + strings.Repeat("2", 64) + `"`},
		{"key_id", `"key_id": "` + cp.KeyID + `"`, `"key_id": "` + strings.Repeat("3", 64) + `"`},
		{"prev_size", fmt.Sprintf(`"prev_size": %d`, cp.PrevSize), fmt.Sprintf(`"prev_size": %d`, cp.PrevSize+1)},
		{"prev_root", `"prev_root": "` + cp.PrevRoot + `"`, `"prev_root": "` + strings.Repeat("4", 64) + `"`},
		{"timestamp", `"timestamp": "` + cp.Timestamp + `"`, `"timestamp": "2020-01-01T00:00:00Z"`},
	} {
		t.Run("head JSON "+tamper.field+" disagrees with its signed statement", func(t *testing.T) {
			tampered := replaceOnce(t, chain, tamper.old, tamper.replacement)
			serve = func([]byte) (int, []byte) { return http.StatusOK, tampered }
			_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
			require.ErrorIs(t, e, ErrConflict)
		})
	}
	t.Run("head with a tampered signed statement", func(t *testing.T) {
		cose := chainVectorLink(t, chain).CheckpointCOSE
		tampered := replaceOnce(t, chain, `"`+cose+`"`, `"`+flipLastHexByte(t, cose)+`"`)
		serve = func([]byte) (int, []byte) { return http.StatusOK, tampered }
		_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
		require.ErrorIs(t, e, ErrPartial)
	})
	t.Run("unreachable party is not an answer and its text is not printed", func(t *testing.T) {
		serve = func([]byte) (int, []byte) {
			return http.StatusBadGateway, []byte("could not reach peer: secret-looking-detail")
		}
		_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
		require.ErrorIs(t, e, ErrNoAnswer)
		assert.NotContains(t, SafeError(e), "secret-looking-detail")
		assert.Equal(t, ErrNoAnswer.Error(), SafeError(e))
	})
	t.Run("an answer over the size bound is not read as an answer", func(t *testing.T) {
		huge := append([]byte(`{"bundles":["`), bytes.Repeat([]byte("a"), maxMeshAnswerBytes)...)
		huge = append(huge, []byte(`"],"subject_kind":"range","v":1}`)...)
		serve = func([]byte) (int, []byte) { return http.StatusOK, huge }
		_, e := invoke(t, "", "book", "list", "--profile", "node", "--party", "peer-a", "--selector", "a..b")
		require.ErrorIs(t, e, ErrNoAnswer)
	})
	t.Run("neither artifact nor refusal", func(t *testing.T) {
		serve = func([]byte) (int, []byte) { return http.StatusOK, []byte(`{"status":"ok"}`) }
		_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
		require.ErrorIs(t, e, ErrPartial)
	})
	for name, body := range map[string]string{
		"outside the four subjects": `{"subject":{"kind":"everything"}}`,
		"with a fractional number":  `{"subject":{"kind":"chain_segment","last":1.5}}`,
		"with an exponent number":   `{"subject":{"kind":"chain_segment","last":1e2}}`,
		"with negative zero":        `{"subject":{"kind":"chain_segment","last":-0}}`,
		"with an integer over u64":  `{"subject":{"kind":"chain_segment","last":99999999999999999999}}`,
		"with an integer under i64": `{"subject":{"kind":"chain_segment","last":-9223372036854775809}}`,
		"with non-ASCII text":       `{"subject":{"kind":"record","capsule_id":"caf\u00e9"}}`,
		"with an escaped newline":   `{"subject":{"kind":"record","capsule_id":"a\nb"}}`,
	} {
		t.Run("request "+name+" is not sent", func(t *testing.T) {
			var calls int
			serve = func([]byte) (int, []byte) { calls++; return http.StatusOK, nil }
			bad := filepath.Join(t.TempDir(), "req.json")
			require.NoError(t, os.WriteFile(bad, []byte(body), 0o600))
			_, e := invoke(t, "", "book", "request", "--profile", "node", "--party", "peer-a", "--request", bad)
			require.ErrorIs(t, e, ErrInput)
			assert.Zero(t, calls)
		})
	}
	t.Run("HTML characters are sent as themselves, the way the node re-encodes them", func(t *testing.T) {
		var sent []byte
		serve = func(r []byte) (int, []byte) { sent = r; return http.StatusOK, evidenceVector(t, "range.answer.json") }
		_, e := invoke(t, "", "book", "list", "--profile", "node", "--party", "peer-a", "--selector", "a<b&c")
		require.NoError(t, e)
		assert.Equal(t, `{"subject":{"kind":"range","selector":"a<b&c"}}`, string(sent))
	})
	t.Run("the integer range edges are sent as themselves", func(t *testing.T) {
		for _, n := range []string{"18446744073709551615", "-9223372036854775808"} {
			var sent []byte
			serve = func(r []byte) (int, []byte) {
				sent = r
				return http.StatusOK, evidenceVector(t, "chain_segment.answer.json")
			}
			req := filepath.Join(t.TempDir(), "req.json")
			body := `{"subject":{"kind":"chain_segment","last":` + n + `}}`
			require.NoError(t, os.WriteFile(req, []byte(body), 0o600))
			_, e := invoke(t, "", "book", "request", "--profile", "node", "--party", "peer-a", "--request", req)
			require.NoError(t, e)
			assert.Equal(t, body, string(sent))
		}
	})
	t.Run("party id must be printable and present", func(t *testing.T) {
		for _, party := range []string{"", "peer a", "peer\n"} {
			_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", party)
			require.ErrorIs(t, e, ErrInput, "%q", party)
		}
	})
}

func TestBookVerbsNeverFollowARedirect(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var leaked []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.Header.Get("Authorization"))
	}))
	t.Cleanup(elsewhere.Close)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+meshEvidenceRequestPath, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(node.Close)
	meshProfile(t, node.URL, "--token", "node-token-value")
	_, e := invoke(t, "", "book", "head", "--profile", "node", "--party", "peer-a")
	require.ErrorIs(t, e, ErrNoAnswer)
	assert.Empty(t, leaked)
}

func TestBookVerbsNeedARemoteProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv, seen := fakeNode(t, serveVectors(t))
	// A storage profile that happens to carry a URL still has no evidence door.
	_, e := invoke(t, "", "profile", "create", "--name", "local", "--type", "jsonl", "--jsonl-path", t.TempDir(), "--log-id", "log-a", "--url", srv.URL)
	require.NoError(t, e)
	_, e = invoke(t, "", "book", "head", "--profile", "local", "--party", "peer-a")
	require.ErrorIs(t, e, ErrInput)
	assert.Empty(t, *seen)
}

// TestMeshVocabularyStaysInTheAdapter keeps the generic verbs transport
// neutral: only meshplugin.go may name the node's plugin, tool or arguments.
func TestMeshVocabularyStaysInTheAdapter(t *testing.T) {
	files, e := filepath.Glob("*.go")
	require.NoError(t, e)
	for _, f := range files {
		if f == "meshplugin.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, e := os.ReadFile(f)
		require.NoError(t, e)
		if f == "evidencedoor.go" || f == "profile.go" || f == "command.go" {
			assert.NotContains(t, strings.ToLower(string(b)), "mesh", "%s is transport-neutral; mesh names belong in meshplugin.go", f)
		}
		for _, word := range []string{"admission-policy", "mesh_", "mesh-plugin", "peer_id"} {
			assert.NotContains(t, string(b), word, "%s names %q; that belongs in meshplugin.go", f, word)
		}
	}
}

// replaceOnce swaps one exact JSON member in a vector and fails the test if
// that member is not present exactly once, so a tamper can never be a no-op.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func compactJSON(t *testing.T, doc []byte) json.RawMessage {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, json.Compact(&out, doc))
	return out.Bytes()
}

func replaceOnce(t *testing.T, doc []byte, old, replacement string) []byte {
	t.Helper()
	count := bytes.Count(doc, []byte(old))
	require.Equal(t, 1, count, "%s", old)
	return bytes.Replace(doc, []byte(old), []byte(replacement), 1)
}

func flipLastHexByte(t *testing.T, value string) string {
	t.Helper()
	raw, e := hex.DecodeString(value)
	require.NoError(t, e)
	raw[len(raw)-1] ^= 1
	return hex.EncodeToString(raw)
}

type vectorCheckpoint struct {
	LogID     string `json:"log_id"`
	MMRSize   uint64 `json:"mmr_size"`
	Root      string `json:"root"`
	KeyID     string `json:"key_id"`
	PrevSize  uint64 `json:"prev_size"`
	PrevRoot  string `json:"prev_root"`
	Timestamp string `json:"timestamp"`
}

type vectorLink struct {
	Checkpoint     vectorCheckpoint `json:"checkpoint"`
	CheckpointCOSE string           `json:"checkpoint_cose"`
}

// chainVectorLink decodes the one link of the chain_segment vector.
func chainVectorLink(t *testing.T, doc []byte) vectorLink {
	t.Helper()
	var a struct {
		Bundles []struct {
			Links []vectorLink `json:"links"`
		} `json:"bundles"`
	}
	require.NoError(t, json.Unmarshal(doc, &a))
	require.Len(t, a.Bundles, 1)
	require.Len(t, a.Bundles[0].Links, 1)
	return a.Bundles[0].Links[0]
}

// A remote profile holds no storage: the storage verbs refuse it before any
// connection, and nothing reaches the node.
func TestStorageVerbsRejectARemoteProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv, seen := fakeNode(t, serveVectors(t))
	p, _ := profileFixture(t)
	meshProfile(t, srv.URL, "--log-id", "log-a", "--signing-key", p.Signing.Value, "--trusted-key", p.TrustedKeys[0])
	request := filepath.Join(t.TempDir(), "request.json")
	sealRequest := requestFixture(t)
	require.NoError(t, os.WriteFile(request, sealRequest, 0o600))
	for _, args := range [][]string{
		{"store", "init"},
		{"cll", "list"},
		{"publish", "--request", request},
	} {
		_, e := invoke(t, "", append(args, "--profile", "node")...)
		require.ErrorIs(t, e, ErrInput, "%v", args)
		assert.Contains(t, e.Error(), "unsupported profile type", "%v", args)
	}
	assert.Empty(t, *seen)
}
