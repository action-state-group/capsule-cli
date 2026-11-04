package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// The commitment under test is a Goose node's own evidence-commitment/1 (a
// byte copy checked by gooseCommitmentSHA256), so the stapled card, its
// checkpoints and the commitment's signature are the real ones. Other
// commitments are that one re-signed under a test key, which changes who
// signed it and when and nothing in the staples.

func gooseOctets(t *testing.T) []byte {
	t.Helper()
	return checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
}

// resign is the Goose commitment with some members changed and signed under
// a fresh key, whose key_id and spki it carries.
func resign(t *testing.T, change func(doc map[string]any)) []byte {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return resignWith(t, private, change)
}

func resignWith(t *testing.T, private ed25519.PrivateKey, change func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(gooseOctets(t), &doc))
	public := private.Public().(ed25519.PublicKey)
	der, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	doc["key_id"], doc["spki"] = hex.EncodeToString(public), base64.StdEncoding.EncodeToString(der)
	restaple(t, doc, private)
	if change != nil {
		change(doc)
	}
	delete(doc, "sig")
	body, err := canonical.JCS(doc)
	require.NoError(t, err)
	doc["sig"] = map[string]any{"alg": "Ed25519", "canon": "jcs", "value": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, body))}
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

// restaple signs the commitment's stapled card envelope under the node's own
// key, as a real node's staple is: same card, same checkpoints, a responder
// that is the commitment's key.
func restaple(t *testing.T, doc map[string]any, key ed25519.PrivateKey) {
	t.Helper()
	card := doc["history_cards"].([]any)[0].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(card["bytes"].(string))
	require.NoError(t, err)
	env := mustDecode(t, raw).(map[string]any)
	artifact := env["artifact"]
	delete(env, "sig")
	delete(env, "artifact")
	env["responder"] = hex.EncodeToString(key.Public().(ed25519.PublicKey))
	body, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	env["sig"] = hex.EncodeToString(ed25519.Sign(key, body))
	env["artifact"] = artifact
	out, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	card["bytes"], card["sha256"] = base64.StdEncoding.EncodeToString(out), sha(out)
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// carrierServer serves a directory record, an A2A card and the commitment.
type carrierServer struct {
	*httptest.Server
	octets []byte
}

func newCarrierServer(t *testing.T, octets []byte, digest string) *carrierServer {
	t.Helper()
	cs := &carrierServer{octets: octets}
	mux := http.NewServeMux()
	cs.Server = httptest.NewTLSServer(mux)
	t.Cleanup(cs.Close)
	old := commitmentClient
	commitmentClient = cs.Client()
	t.Cleanup(func() { commitmentClient = old })
	mux.HandleFunc("/commitment.json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(cs.octets) })
	mux.HandleFunc("/ard.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": []any{map[string]any{"trustManifest": map[string]any{
			"identity": cs.URL,
			"attestations": []any{map[string]any{
				"type": "evidence-commitment/1", "uri": cs.URL + "/commitment.json", "mediaType": "application/json",
				"digest": "sha256:" + digest, "description": "verify the commitment, not this entry",
			}},
		}}}})
	})
	mux.HandleFunc("/card.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": map[string]any{"extensions": []any{
			map[string]any{"uri": a2aEvidenceRequestExtension, "required": false, "params": map[string]any{"profiles": []string{"evidence-interop/1"}}},
			map[string]any{"uri": a2aCommitmentExtension, "required": false, "params": map[string]any{"commitment_uri": cs.URL + "/commitment.json", "commitment_digest": "sha256:" + digest}},
		}}})
	})
	return cs
}

func find(t *testing.T, args ...string) (findReport, error) {
	t.Helper()
	out, err := invoke(t, "", append([]string{"commitment", "find"}, args...)...)
	var rep findReport
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	}
	return rep, err
}

func TestFindFromDirectoryAndA2AVerifiesTheSameCommitment(t *testing.T) {
	octets := gooseOctets(t)
	cs := newCarrierServer(t, octets, sha(octets))
	pins := t.TempDir()
	rep, err := find(t, "--directory", cs.URL+"/ard.json", "--a2a-card", cs.URL+"/card.json", "--node-key", gooseNodeKey, "--pin-dir", pins)
	require.NoError(t, err)
	require.Len(t, rep.Nodes, 1, "one commitment through two carriers is verified once")
	n := rep.Nodes[0]
	assert.Equal(t, "VERIFIED", rep.Verdict)
	assert.Equal(t, "current", n.Commitment.Status)
	assert.ElementsMatch(t, []string{"directory", "a2a"}, n.Commitment.Sources)
	assert.Equal(t, "PASS", n.KeyBinding["node_key"])
	assert.Equal(t, "unwitnessed", n.Witness.Label, "no receipt was given: the label is derived, not set by the node")
	assert.Contains(t, n.Witness.Reason, "no receipt")
	assert.Contains(t, n.Live, "NOT_CHECKED", "the Goose node names no http endpoint to ask")
	assert.NotEmpty(t, n.Pin)
}

func TestFindAStalePointerIsNotAForgery(t *testing.T) {
	octets := gooseOctets(t)
	cs := newCarrierServer(t, octets, strings.Repeat("0", 64))
	rep, err := find(t, "--directory", cs.URL+"/ard.json", "--node-key", gooseNodeKey, "--no-pin")
	require.NoError(t, err)
	assert.Equal(t, []string{"directory"}, rep.Nodes[0].Commitment.DigestMismatch)
	assert.Contains(t, strings.Join(rep.Nodes[0].Findings, "\n"), reasonDigestMismatch)
	assert.Equal(t, "VERIFIED", rep.Verdict, "the octets verify on their own (V1)")
}

func TestFindRefusesAlteredOctetsWhateverTheCarrier(t *testing.T) {
	var doc map[string]any
	require.NoError(t, json.Unmarshal(gooseOctets(t), &doc))
	doc["answers_within"] = "PT1H"
	altered, err := json.Marshal(doc)
	require.NoError(t, err)
	cs := newCarrierServer(t, altered, sha(altered))
	rep, err := find(t, "--directory", cs.URL+"/ard.json", "--no-pin")
	require.ErrorIs(t, err, ErrHistoryNotAccepted)
	require.Len(t, rep.Refused, 1)
	assert.Equal(t, reasonCommitmentSignature, rep.Refused[0].Refused)
	assert.Empty(t, rep.Nodes)
}

func TestFindRefusesAStapleDigestThatIsNotTheCardBytes(t *testing.T) {
	bad := resign(t, func(doc map[string]any) {
		doc["history_cards"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("1", 64)
	})
	cs := newCarrierServer(t, bad, sha(bad))
	rep, err := find(t, "--directory", cs.URL+"/ard.json", "--no-pin")
	require.ErrorIs(t, err, ErrHistoryNotAccepted)
	assert.Equal(t, reasonStapleDigest, rep.Refused[0].Refused)
}

func TestFindKeyMismatchAndForgedKeyID(t *testing.T) {
	octets := resign(t, nil)
	cs := newCarrierServer(t, octets, sha(octets))
	t.Run("a valid commitment under another key does not describe the node", func(t *testing.T) {
		rep, err := find(t, "--directory", cs.URL+"/ard.json", "--node-key", gooseNodeKey, "--no-pin")
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		assert.Contains(t, rep.Nodes[0].Reasons, reasonKeyMismatch)
		assert.Equal(t, "FAIL", rep.Nodes[0].KeyBinding["node_key"])
	})
	t.Run("a key_id that is not the key in spki is refused", func(t *testing.T) {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(octets, &doc))
		doc["key_id"] = gooseNodeKey
		forged, err := json.Marshal(doc)
		require.NoError(t, err)
		d := checkCommitmentOctets(forged)
		assert.Equal(t, reasonKeyIDSPKI, d.Refused)
	})
}

func TestRankCommitmentsNewestWins(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	at := func(ts string, mark string) *commitmentDoc {
		return &commitmentDoc{KeyID: "k", NodeID: "n", issued: mustTime(t, ts), SHA256: mark}
	}
	t.Run("the later issued_at is current and the earlier superseded", func(t *testing.T) {
		old, newer := at("2026-11-04T10:00:00Z", "a"), at("2026-11-04T11:00:00Z", "b")
		rankCommitments([]*commitmentDoc{old, newer}, now, commitmentSkew)
		assert.Equal(t, "superseded", old.Status)
		assert.Equal(t, "current", newer.Status)
	})
	t.Run("one issued_at, two octets: conflicting until a later one is read", func(t *testing.T) {
		a, b := at("2026-11-04T10:00:00Z", "a"), at("2026-11-04T10:00:00Z", "b")
		rankCommitments([]*commitmentDoc{a, b}, now, commitmentSkew)
		assert.Equal(t, "conflicting", a.Status)
		assert.Equal(t, "conflicting", b.Status)
		later := at("2026-11-04T11:00:00Z", "c")
		rankCommitments([]*commitmentDoc{a, b, later}, now, commitmentSkew)
		assert.Equal(t, "current", later.Status)
		assert.Equal(t, "superseded", a.Status)
	})
	t.Run("a future-dated commitment takes no part, one at the skew limit does", func(t *testing.T) {
		good, future := at("2026-11-04T11:00:00Z", "g"), at("2026-11-04T12:05:01Z", "f")
		edge := at("2026-11-04T12:05:00Z", "e")
		rankCommitments([]*commitmentDoc{good, future}, now, commitmentSkew)
		assert.Equal(t, "future_dated", future.Status)
		assert.Equal(t, "current", good.Status)
		rankCommitments([]*commitmentDoc{good, edge}, now, commitmentSkew)
		assert.Equal(t, "current", edge.Status)
	})
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}

func TestFindTakesTheNewerCommitmentFromAnotherCarrier(t *testing.T) {
	older := resign(t, func(doc map[string]any) { doc["issued_at"] = "2026-11-01T00:00:00Z" })
	cs := newCarrierServer(t, older, sha(older))
	path := filepath.Join(t.TempDir(), "newer.json")
	require.NoError(t, os.WriteFile(path, gooseOctets(t), 0o600))
	rep, err := find(t, "--directory", cs.URL+"/ard.json", "--file", path, "--no-pin", "--node-key", gooseNodeKey)
	require.NoError(t, err, "a stranger's valid commitment under another key for the node cannot change the verdict")
	assert.Len(t, rep.Nodes, 2, "two keys are two nodes here; the key decides who is the node")
	assert.Equal(t, "VERIFIED", rep.Verdict)
	verdicts := []string{rep.Nodes[0].Verdict, rep.Nodes[1].Verdict}
	assert.ElementsMatch(t, []string{"VERIFIED", "NOT_DESCRIBING"}, verdicts)

	_, err = find(t, "--directory", cs.URL+"/ard.json", "--no-pin", "--node-key", strings.Repeat("3", 64))
	require.ErrorIs(t, err, ErrHistoryNotAccepted, "no commitment is under the key the asker holds")
}

func TestFindFirstCheckpointPin(t *testing.T) {
	octets := gooseOctets(t)
	pins := t.TempDir()
	path := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, os.WriteFile(path, octets, 0o600))
	args := []string{"--file", path, "--node-key", gooseNodeKey, "--pin-dir", pins}

	rep, err := find(t, args...)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(rep.Nodes[0].Pin, "\n"), "pinned the first checkpoint")
	pinFile := filepath.Join(pins, gooseNodeKey+".json")
	raw, err := os.ReadFile(pinFile)
	require.NoError(t, err, "an unwitnessed node's first checkpoint is stored per node key")
	var stored commitmentPins
	require.NoError(t, json.Unmarshal(raw, &stored))
	pin := stored.Streams["goose/node"]
	assert.NotEmpty(t, pin.Root)
	assert.NotZero(t, pin.Size)

	t.Run("the same chain extends its own pin", func(t *testing.T) {
		rep, err := find(t, args...)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(rep.Nodes[0].Pin, "\n"), "extends the checkpoint pinned")
	})
	t.Run("an answer that cannot prove consistency to the pin fails", func(t *testing.T) {
		for name, mutate := range map[string]func(p *streamPin){
			"a different root at the pinned size": func(p *streamPin) { p.Root = strings.Repeat("a", 64) },
			"a log shorter than the pin":          func(p *streamPin) { p.Size += 1000 },
			"a different log":                     func(p *streamPin) { p.LogID = "other" },
		} {
			t.Run(name, func(t *testing.T) {
				p := stored
				p.Streams = map[string]streamPin{"goose/node": pin}
				changed := p.Streams["goose/node"]
				mutate(&changed)
				p.Streams["goose/node"] = changed
				data, err := json.Marshal(p)
				require.NoError(t, err)
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, gooseNodeKey+".json"), data, 0o600))
				rep, err := find(t, "--file", path, "--node-key", gooseNodeKey, "--pin-dir", dir)
				require.ErrorIs(t, err, ErrHistoryNotAccepted)
				assert.Contains(t, rep.Nodes[0].Reasons, "pin_consistency")
			})
		}
	})
	t.Run("a stream missing from a later commitment is dropped, never silent", func(t *testing.T) {
		p := stored
		p.Streams = map[string]streamPin{"goose/node": pin, "old/stream": {LogID: "x", Size: 1, Root: "r", SeenAt: "2026-10-01T00:00:00Z"}}
		data, err := json.Marshal(p)
		require.NoError(t, err)
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, gooseNodeKey+".json"), data, 0o600))
		rep, err := find(t, "--file", path, "--node-key", gooseNodeKey, "--pin-dir", dir)
		require.NoError(t, err)
		assert.Contains(t, strings.Join(rep.Nodes[0].Pin, "\n"), "dropped since 2026-10-01T00:00:00Z: old/stream")
	})
	t.Run("a failed answer pins nothing", func(t *testing.T) {
		dir := t.TempDir()
		_, err := find(t, "--file", path, "--node-key", strings.Repeat("e", 64), "--pin-dir", dir)
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		_, statErr := os.Stat(filepath.Join(dir, gooseNodeKey+".json"))
		assert.True(t, os.IsNotExist(statErr))
	})
}

func TestFindAdmissionCanRefuseTheUnwitnessed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, os.WriteFile(path, gooseOctets(t), 0o600))
	rep, err := find(t, "--file", path, "--node-key", gooseNodeKey, "--no-pin", "--refuse-unwitnessed")
	require.ErrorIs(t, err, ErrHistoryNotAccepted)
	assert.Contains(t, rep.Nodes[0].Reasons, "unwitnessed_refused")
	rep, err = find(t, "--file", path, "--node-key", gooseNodeKey, "--no-pin")
	require.NoError(t, err, "the default admits the unwitnessed, labelled")
	assert.Equal(t, "unwitnessed", rep.Nodes[0].Witness.Label)
}

func TestFindInputErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"nothing to look at":      {},
		"a relay needs a node id": {"--nostr-relay", "wss://relay.invalid"},
		"a bad node id":           {"--file", "x", "--node-id", "abc"},
		"no-pin and pin-dir":      {"--file", "x", "--no-pin", "--pin-dir", "/tmp/x"},
		"a non-wss relay":         {"--nostr-relay", "ws://relay.invalid", "--node-id", strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := find(t, args...)
			require.ErrorIs(t, err, ErrInput)
		})
	}
}

func TestCarrierEntryForms(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	t.Run("a directory entry with a size is a count", func(t *testing.T) {
		_, fails := directoryPointers([]byte(`{"trust_manifest":{"attestations":[{"type":"evidence-commitment/1","uri":"https://x/c","mediaType":"application/json","digest":"` + digest + `","size":10}]}}`))
		require.Len(t, fails, 1)
		assert.Equal(t, checkDirectoryEntry, fails[0].Check)
	})
	t.Run("an entry of another type is left alone", func(t *testing.T) {
		pointers, fails := directoryPointers([]byte(`{"trustManifest":{"attestations":[{"type":"other/1","size":3}]}}`))
		assert.Empty(t, pointers)
		assert.Empty(t, fails)
	})
	t.Run("an A2A entry with required set is refused", func(t *testing.T) {
		card := `{"capabilities":{"extensions":[{"uri":"` + a2aCommitmentExtension + `","required":true,"params":{"commitment_uri":"https://x/c","commitment_digest":"` + digest + `"}}]}}`
		_, fails := a2aPointers([]byte(card))
		require.Len(t, fails, 1)
		assert.Equal(t, checkA2AEntry, fails[0].Check)
	})
	t.Run("a commitment inside the evidence-request entry is a discovery object", func(t *testing.T) {
		card := `{"capabilities":{"extensions":[{"uri":"` + a2aEvidenceRequestExtension + `","required":false,"params":{"issued_at":"2026-11-04T00:00:00Z"}}]}}`
		pointers, fails := a2aPointers([]byte(card))
		assert.Empty(t, pointers)
		require.Len(t, fails, 1)
		assert.Equal(t, checkDiscoveryObject, fails[0].Check)
	})
}

// signedEvent is a NIP-01 announce event by a fresh mailbox key.
func signedEvent(t *testing.T, key *btcec.PrivateKey, kind int, tags [][]string, content string) nostrEvent {
	t.Helper()
	ev := nostrEvent{Pubkey: hex.EncodeToString(schnorr.SerializePubKey(key.PubKey())), CreatedAt: 1791029160, Kind: kind, Tags: tags, Content: content}
	pre, err := ev.serialize()
	require.NoError(t, err)
	sum := sha256.Sum256(pre)
	ev.ID = hex.EncodeToString(sum[:])
	sig, err := schnorr.Sign(key, sum[:])
	require.NoError(t, err)
	ev.Sig = hex.EncodeToString(sig.Serialize())
	return ev
}

func relayServing(t *testing.T, events ...nostrEvent) string {
	t.Helper()
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var req []json.RawMessage
		if websocket.JSON.Receive(ws, &req) != nil || len(req) < 2 {
			return
		}
		var sub string
		_ = json.Unmarshal(req[1], &sub)
		for _, ev := range events {
			_ = websocket.JSON.Send(ws, []any{"EVENT", sub, ev})
		}
		_ = websocket.JSON.Send(ws, []any{"EOSE", sub})
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	old := nostrDialInsecure
	nostrDialInsecure = true
	t.Cleanup(func() { nostrDialInsecure = old })
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestFindFromANostrRelay(t *testing.T) {
	octets := gooseOctets(t)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	node := "8c0c0699a2514ab17971f9596d05cd2ef121f4b21f5aadef34d42edb5a018088"
	tags := [][]string{{"d", node}, {"t", "evidence-commitment"}, {"x", sha(octets)}}
	good := signedEvent(t, key, nostrAnnounceKind, tags, base64.RawURLEncoding.EncodeToString(octets))

	t.Run("an announce event carries the octets", func(t *testing.T) {
		relay := relayServing(t, good)
		rep, err := find(t, "--nostr-relay", relay, "--allow-private-network", "--node-id", node, "--node-key", gooseNodeKey, "--no-pin")
		require.NoError(t, err)
		assert.Equal(t, []string{"nostr"}, rep.Nodes[0].Commitment.Sources)
	})
	t.Run("an event whose signature does not verify is not read", func(t *testing.T) {
		bad := good
		bad.Sig = strings.Repeat("0", 128)
		relay := relayServing(t, bad)
		rep, err := find(t, "--nostr-relay", relay, "--allow-private-network", "--node-id", node, "--no-pin")
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		assert.Empty(t, rep.Nodes)
		require.Len(t, rep.Failures, 1, "a bad event is named, not dropped")
		assert.Equal(t, checkNostrEvent, rep.Failures[0].Check)
	})
	t.Run("an event with the wrong tags fails its form", func(t *testing.T) {
		wrong := signedEvent(t, key, nostrAnnounceKind, [][]string{{"d", node}, {"x", sha(octets)}}, base64.RawURLEncoding.EncodeToString(octets))
		relay := relayServing(t, wrong)
		rep, err := find(t, "--nostr-relay", relay, "--allow-private-network", "--node-id", node, "--no-pin")
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		require.Len(t, rep.Failures, 1)
		assert.Equal(t, checkNostrEvent, rep.Failures[0].Check)
	})
	t.Run("a forged copy re-posted under another key is not trusted for its author", func(t *testing.T) {
		other, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		relayed := signedEvent(t, other, nostrAnnounceKind, tags, base64.RawURLEncoding.EncodeToString(octets))
		relay := relayServing(t, relayed)
		d := checkCommitmentOctets(octets)
		assert.Equal(t, "relayed", nostrAttribution(d, &relayed), "the author is not the commitment's mailbox key")
		rep, err := find(t, "--nostr-relay", relay, "--allow-private-network", "--node-id", node, "--node-key", gooseNodeKey, "--no-pin")
		require.NoError(t, err, "the octets are the same and verify on their own")
		assert.Equal(t, "VERIFIED", rep.Verdict)
	})
}

// bech32Encode is BIP-173's encoder, for building test addresses.
func bech32Encode(hrp string, payload []byte) string {
	data, _ := convertBitsPad(payload, 8, 5)
	exp := []byte{}
	for _, c := range []byte(hrp) {
		exp = append(exp, c>>5)
	}
	exp = append(exp, 0)
	for _, c := range []byte(hrp) {
		exp = append(exp, c&31)
	}
	mod := bech32Polymod(append(append(exp, data...), 0, 0, 0, 0, 0, 0)) ^ 1
	out := hrp + "1"
	for _, d := range data {
		out += string(bech32Charset[d])
	}
	for i := 0; i < 6; i++ {
		out += string(bech32Charset[byte(mod>>uint(5*(5-i)))&31])
	}
	return out
}

func convertBitsPad(data []byte, from, to uint) ([]byte, bool) {
	var acc, bits uint
	var out []byte
	for _, v := range data {
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&(1<<to-1)))
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(to-bits)&(1<<to-1)))
	}
	return out, true
}

func TestBech32ChecksumVectors(t *testing.T) {
	// BIP-173's valid strings: the checksum code is the standard one.
	for hrp, str := range map[string]string{"a": "a12uel5l", "abcdef": "abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw"} {
		var data []byte
		for _, c := range str[len(hrp)+1:] {
			data = append(data, byte(strings.IndexRune(bech32Charset, c)))
		}
		assert.True(t, bech32Verify(hrp, data), str)
		data[0] ^= 1
		assert.False(t, bech32Verify(hrp, data), str+" with a changed character")
	}
}

func TestParseNaddr(t *testing.T) {
	author := strings.Repeat("ab", 32)
	authorBytes, _ := hex.DecodeString(author)
	node := strings.Repeat("c", 64)
	tlv := func(typ byte, v []byte) []byte { return append([]byte{typ, byte(len(v))}, v...) }
	payload := append(tlv(0, []byte(node)), tlv(1, []byte("wss://relay.example"))...)
	payload = append(payload, tlv(2, authorBytes)...)
	payload = append(payload, tlv(3, []byte{0, 0, 0x88, 0x5c})...) // 34908? set below
	payload[len(payload)-2], payload[len(payload)-1] = byte(nostrAnnounceKind>>8), byte(nostrAnnounceKind&0xff)
	payload[len(payload)-3], payload[len(payload)-4] = 0, 0
	uri := "nostr:" + bech32Encode("naddr", payload)

	got, err := parseNaddr(uri)
	require.NoError(t, err)
	assert.Equal(t, naddr{D: node, Relays: []string{"wss://relay.example"}, Author: author, Kind: nostrAnnounceKind}, got)

	corrupt := uri[:len(uri)-1] + map[bool]string{true: "p", false: "q"}[!strings.HasSuffix(uri, "p")]
	_, err = parseNaddr(corrupt)
	assert.Error(t, err, "a changed character fails the checksum")
	_, err = parseNaddr("nostr:npub1abc")
	assert.Error(t, err)
}

// The witness label is derived from receipts under the asker's own policy,
// through the anchor-witness-v1 vectors' receipt: a receipt by a witness the
// policy lists makes "witnessed since" its registration time; the same receipt
// under a policy that does not list that witness, or over another anchor,
// makes the node unwitnessed. Nothing the node says is an input.
func TestDeriveWitnessLabel(t *testing.T) {
	var file struct {
		Policies map[string]witness.AnchorWitnessPolicy `json:"policies"`
		Cases    []struct {
			Name     string   `json:"name"`
			Anchor   string   `json:"anchor"`
			Receipts []string `json:"receipts"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(pinnedVectors(t, "anchor-witness-v1", anchorWitnessVectorsSHA256), &file))
	var anchor string
	var receipt []byte
	for _, c := range file.Cases {
		if c.Name == "pass-eddsa-time" {
			anchor = c.Anchor
			var err error
			receipt, err = hex.DecodeString(c.Receipts[0])
			require.NoError(t, err)
		}
	}
	require.NotEmpty(t, anchor)
	rows := []cardRow{{stapleRow: stapleRow{Stream: "s"}, anchor: anchor, responder: "k"}}

	got := deriveWitness(rows, "k", findInput{Receipts: [][]byte{receipt}, Policy: file.Policies["default"]})
	assert.Equal(t, "witnessed since "+got.Since, got.Label)
	assert.NotEmpty(t, got.Since)
	assert.Equal(t, "PASS", got.Streams[0].Status)

	none := deriveWitness(rows, "k", findInput{})
	assert.Equal(t, "unwitnessed", none.Label)
	assert.Contains(t, none.Reason, "no receipt")

	unknown := deriveWitness(rows, "k", findInput{Receipts: [][]byte{receipt}})
	assert.Equal(t, "unwitnessed", unknown.Label, "a receipt by a witness the asker's policy does not list counts for nothing")
	assert.Contains(t, unknown.Reason, "policy")

	other := deriveWitness([]cardRow{{stapleRow: stapleRow{Stream: "s"}, anchor: strings.Repeat("0", 64), responder: "k"}}, "k", findInput{Receipts: [][]byte{receipt}, Policy: file.Policies["default"]})
	assert.Equal(t, "unwitnessed", other.Label, "a receipt over another anchor is not this card's")

	foreign := deriveWitness(rows, "someone-else", findInput{Receipts: [][]byte{receipt}, Policy: file.Policies["default"]})
	assert.Equal(t, "unwitnessed", foreign.Label, "a card another key signed earns this key no label")
	assert.Contains(t, foreign.Reason, "another key")

	late := append(rows, cardRow{stapleRow: stapleRow{Stream: "t"}, anchor: anchor, responder: "k"})
	assert.Equal(t, "witnessed since "+got.Since, deriveWitness(late, "k", findInput{Receipts: [][]byte{receipt}, Policy: file.Policies["default"]}).Label)
}

// nodeStub answers a POSTed history_card/1 request the way a node does: the
// Goose card (its real checkpoints) as a fresh answer to the checkpoints
// subject, in an envelope bound to the request and signed by key.
func nodeStub(t *testing.T, key ed25519.PrivateKey, mutate func(card map[string]any)) http.HandlerFunc {
	t.Helper()
	var doc struct {
		Cards []struct {
			Bytes string `json:"bytes"`
		} `json:"history_cards"`
	}
	require.NoError(t, json.Unmarshal(gooseOctets(t), &doc))
	staple, err := base64.StdEncoding.DecodeString(doc.Cards[0].Bytes)
	require.NoError(t, err)
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: staple, MediaType: evidencerequest.MediaTypeJSON})
	require.NoError(t, err)
	card := mustDecode(t, reply.Envelope.Artifact).(map[string]any)
	return func(w http.ResponseWriter, r *http.Request) {
		request, _ := io.ReadAll(r.Body)
		var req struct {
			Coverage struct {
				MinFreshness uint64 `json:"min_freshness"`
			} `json:"coverage"`
		}
		_ = json.Unmarshal(request, &req)
		fresh := map[string]any{}
		for k, v := range card {
			fresh[k] = v
		}
		fresh["subject"] = map[string]any{"checkpoints": nil}
		if mutate != nil {
			mutate(fresh)
		}
		artifact, err := evidencerequest.EncodeJCS(fresh)
		require.NoError(t, err)
		requestSum, artifactSum := sha256.Sum256(request), sha256.Sum256(artifact)
		env := map[string]any{
			"profile": evidencerequest.ProfileIDv1, "request_digest": hex.EncodeToString(requestSum[:]),
			"subject": map[string]any{"checkpoints": nil}, "derivation": "history_card/1",
			"anchor":          map[string]any{"digest": fresh["anchor"], "freshness": req.Coverage.MinFreshness},
			"artifact_digest": hex.EncodeToString(artifactSum[:]),
			"responder":       hex.EncodeToString(key.Public().(ed25519.PublicKey)), "issued_at": DealClock().Format("2006-01-02T15:04:05Z"),
		}
		body, err := evidencerequest.EncodeJCS(env)
		require.NoError(t, err)
		env["sig"] = hex.EncodeToString(ed25519.Sign(key, body))
		env["artifact"] = base64.RawURLEncoding.EncodeToString(artifact)
		out, err := evidencerequest.EncodeJCS(env)
		require.NoError(t, err)
		_, _ = w.Write(out)
	}
}

func TestFindAsksTheNodeLive(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var handler http.HandlerFunc
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)
	old := commitmentClient
	commitmentClient = srv.Client()
	t.Cleanup(func() { commitmentClient = old })
	octets := resignWith(t, key, func(doc map[string]any) {
		doc["ask"] = map[string]any{"bindings": []any{"http"}, "endpoint": srv.URL + "/evidence"}
	})
	path := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, os.WriteFile(path, octets, 0o600))
	nodeKey := hex.EncodeToString(key.Public().(ed25519.PublicKey))

	t.Run("a fresh card signed by key_id extends the checkpoint", func(t *testing.T) {
		handler = nodeStub(t, key, nil)
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		require.NoError(t, err)
		n := rep.Nodes[0]
		assert.True(t, strings.HasPrefix(n.Live, "PASS"), n.Live)
		assert.Equal(t, "PASS", n.KeyBinding["answers_under_key_id"])
		assert.Equal(t, "VERIFIED", n.Verdict)
	})
	t.Run("an answer signed by another key does not prove key_id", func(t *testing.T) {
		_, other, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		handler = nodeStub(t, other, nil)
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		assert.Contains(t, rep.Nodes[0].Reasons, "live_answer")
	})
	t.Run("a fresh card of a stream the commitment does not staple checks nothing", func(t *testing.T) {
		handler = nodeStub(t, key, func(card map[string]any) { card["evidence_stream"] = "other/stream" })
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(rep.Nodes[0].Live, "NOT_CHECKED"), rep.Nodes[0].Live)
	})
	t.Run("a failed live answer is reported as FAIL, not PASS", func(t *testing.T) {
		_, other, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		handler = nodeStub(t, other, nil)
		rep, _ := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		assert.True(t, strings.HasPrefix(rep.Nodes[0].Live, "FAIL"), rep.Nodes[0].Live)
	})
	t.Run("a node under another key than the asker holds is not asked", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) { t.Error("the node was asked"); http.Error(w, "", 500) }
		_, err := find(t, "--file", path, "--no-pin", "--node-key", gooseNodeKey)
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
	})
	t.Run("a refusal or another form is not a failure", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"reason":"policy_declined"}`)) }
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(rep.Nodes[0].Live, "NOT_CHECKED"), rep.Nodes[0].Live)
	})
	t.Run("a node that does not answer leaves the stapled chain as the only check", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", nodeKey)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(rep.Nodes[0].Live, "NOT_CHECKED"), rep.Nodes[0].Live)
	})
	t.Run("--offline asks nothing", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) { t.Error("the node was asked"); http.Error(w, "", 500) }
		rep, err := find(t, "--file", path, "--no-pin", "--offline", "--node-key", nodeKey)
		require.NoError(t, err)
		assert.Contains(t, rep.Nodes[0].Live, "--offline")
	})
}

func TestFindWithoutAnyKeyBindsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	require.NoError(t, os.WriteFile(path, gooseOctets(t), 0o600))
	pins := t.TempDir()
	rep, err := find(t, "--file", path, "--pin-dir", pins)
	require.ErrorIs(t, err, ErrPartial, "nothing failed; only the key is left to give")
	assert.Equal(t, "KEY_NOT_BOUND", rep.Verdict)
	assert.Contains(t, rep.Nodes[0].Reasons, "key_unbound")
	entries, _ := os.ReadDir(pins)
	assert.Empty(t, entries, "an unbound commitment pins nothing")
	assert.Contains(t, rep.Nodes[0].Live, "not contacted")
}

func TestStapleFailuresIgnoreAnotherStreamsAnchor(t *testing.T) {
	row := func(anchor string) cardRow {
		return cardRow{stapleRow: stapleRow{Properties: map[string]string{"commitment_anchor": anchor}, Findings: []string{}}}
	}
	assert.Empty(t, stapleFailures([]cardRow{row("PASS"), row("FAIL")}), "the checkpoint is one stream's")
	assert.Contains(t, stapleFailures([]cardRow{row("FAIL"), row("FAIL")}), "staple_commitment_anchor")
}

func TestFindRefusesAMalformedStapleInsteadOfAborting(t *testing.T) {
	bad := resign(t, func(doc map[string]any) { doc["history_cards"] = "x" })
	path := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(path, bad, 0o600))
	rep, err := find(t, "--file", path, "--file", filepath.Join("testdata", "goose-commitment-v1", "commitment.json"), "--no-pin", "--node-key", gooseNodeKey)
	require.NoError(t, err, "one malformed commitment does not hide the other")
	require.Len(t, rep.Refused, 1)
	assert.Equal(t, reasonCommitmentForm, rep.Refused[0].Refused)
}

func TestFindAnA2ACardWithADiscoveryObjectViolationIsNotRead(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	card := `{"capabilities":{"extensions":[{"uri":"` + a2aEvidenceRequestExtension + `","required":false,"params":{"issued_at":"2026-11-04T00:00:00Z"}},` +
		`{"uri":"` + a2aCommitmentExtension + `","required":false,"params":{"commitment_uri":"https://x/c","commitment_digest":"` + digest + `"}}]}}`
	pointers, fails := a2aPointers([]byte(card))
	assert.Empty(t, pointers, "V0 failed: the card's commitment entry is not read")
	assert.Len(t, fails, 1)
}

func TestFindDoesNotReachPrivateHosts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	// The shipped client, not a test client: it refuses a loopback address,
	// and never follows a redirect.
	_, err := fetchHTTPS(http.MethodGet, srv.URL, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a public address")
	assert.NoError(t, publicOnly("tcp", "8.8.8.8:443", nil))
	for _, a := range []string{"127.0.0.1:1", "10.0.0.1:1", "192.168.1.1:1", "169.254.169.254:1", "[::1]:1", "0.0.0.0:1"} {
		assert.Error(t, publicOnly("tcp", a, nil), a)
	}
	allowPrivateAddresses = true
	t.Cleanup(func() { allowPrivateAddresses = false })
	assert.NoError(t, publicOnly("tcp", "10.0.0.1:1", nil))
}

func TestFindDoesNotFollowARedirect(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`secret`)) }))
	t.Cleanup(target.Close)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	t.Cleanup(redirect.Close)
	old := commitmentClient
	commitmentClient = &http.Client{Transport: redirect.Client().Transport, CheckRedirect: old.CheckRedirect}
	t.Cleanup(func() { commitmentClient = old })
	_, err := fetchHTTPS(http.MethodGet, redirect.URL, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "302")
}

func TestPinLockSerializesRuns(t *testing.T) {
	dir := t.TempDir()
	first, err := loadPins(dir, gooseNodeKey)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		first.release()
		close(done)
	}()
	second, err := loadPins(dir, gooseNodeKey)
	require.NoError(t, err, "the second run waits for the first to release")
	second.release()
	<-done
}

func TestAnExistingPinBindsAWitnessedNodeToo(t *testing.T) {
	// pinChecks with create false (the node is witnessed) still checks a pin
	// and makes none.
	pins := &commitmentPins{KeyID: "k", Streams: map[string]streamPin{"s": {LogID: "L", Size: 5, Root: "r", SeenAt: "t"}}}
	var failed []string
	fail := func(reason, _ string) { failed = append(failed, reason) }
	chain := cardChain{Stream: "s", Parsed: []witness.SignedCheckpoint{{LogID: "L", MMRSize: 9, Root: "other"}}}
	notes := pinChecks(pins, []cardChain{chain, {Stream: "new", Parsed: chain.Parsed}}, time.Now(), false, fail)
	assert.Equal(t, []string{"pin_consistency"}, failed, "a witnessed fork of the pinned log still fails")
	assert.NotContains(t, strings.Join(notes, ""), "new stream")
	assert.NotContains(t, pins.Streams, "new")
}

func TestFindReadsAnEvidenceCommitmentZeroAndRefusesDuplicateStreams(t *testing.T) {
	t.Run("a /0 commitment staples one card in history_card, with no stream label", func(t *testing.T) {
		_, key, _ := ed25519.GenerateKey(nil)
		octets := resignWith(t, key, func(doc map[string]any) {
			doc["profile"] = "evidence-commitment/0"
			card := doc["history_cards"].([]any)[0].(map[string]any)
			delete(card, "stream")
			delete(card, "derivation")
			delete(doc, "history_cards")
			doc["history_card"] = card
		})
		path := filepath.Join(t.TempDir(), "c.json")
		require.NoError(t, os.WriteFile(path, octets, 0o600))
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
		require.NoError(t, err)
		assert.Equal(t, "evidence-commitment/0", rep.Nodes[0].Commitment.Profile)
	})
	t.Run("two cards for one stream are refused: the pin and live checks read the card the rows checked", func(t *testing.T) {
		_, key, _ := ed25519.GenerateKey(nil)
		octets := resignWith(t, key, func(doc map[string]any) {
			cards := doc["history_cards"].([]any)
			doc["history_cards"] = append(cards, cards[0])
		})
		path := filepath.Join(t.TempDir(), "c.json")
		require.NoError(t, os.WriteFile(path, octets, 0o600))
		rep, err := find(t, "--file", path, "--no-pin", "--node-key", hex.EncodeToString(key.Public().(ed25519.PublicKey)))
		require.ErrorIs(t, err, ErrHistoryNotAccepted)
		assert.Contains(t, rep.Nodes[0].Reasons, "staple_stream")
	})
}

func TestResolveNaddrChecksTheEventAgainstTheAddress(t *testing.T) {
	octets := gooseOctets(t)
	mine, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	node := strings.Repeat("c", 64)
	tags := [][]string{{"d", node}, {"t", "evidence-commitment"}, {"x", sha(octets)}}
	content := base64.RawURLEncoding.EncodeToString(octets)
	author := hex.EncodeToString(schnorr.SerializePubKey(mine.PubKey()))
	authorBytes, _ := hex.DecodeString(author)
	tlv := func(typ byte, v []byte) []byte { return append([]byte{typ, byte(len(v))}, v...) }
	addr := func(relay string) string {
		payload := append(tlv(0, []byte(node)), tlv(1, []byte(relay))...)
		payload = append(payload, tlv(2, authorBytes)...)
		payload = append(payload, tlv(3, []byte{0, 0, byte(nostrAnnounceKind >> 8), byte(nostrAnnounceKind & 0xff)})...)
		return "nostr:" + bech32Encode("naddr", payload)
	}
	// The relay ignores the filter and serves another author's newer event.
	stranger := signedEvent(t, other, nostrAnnounceKind, tags, content)
	stranger.CreatedAt += 1000
	stranger = resignEvent(t, other, stranger)
	relay := relayServing(t, stranger)
	allowPrivateAddresses = true
	t.Cleanup(func() { allowPrivateAddresses = false })
	_, err = resolvePointer(commitmentPointer{Carrier: "directory", URI: addr(relay)}, 0)
	require.Error(t, err, "an event by another author is not the one the address names")
	assert.Contains(t, err.Error(), "no relay has the event")

	genuine := signedEvent(t, mine, nostrAnnounceKind, tags, content)
	relay = relayServing(t, stranger, genuine)
	got, err := resolvePointer(commitmentPointer{Carrier: "directory", URI: addr(relay)}, 0)
	require.NoError(t, err)
	assert.Equal(t, octets, got)
}

// resignEvent recomputes an event's id and signature after a change.
func resignEvent(t *testing.T, key *btcec.PrivateKey, ev nostrEvent) nostrEvent {
	t.Helper()
	pre, err := ev.serialize()
	require.NoError(t, err)
	sum := sha256.Sum256(pre)
	ev.ID = hex.EncodeToString(sum[:])
	sig, err := schnorr.Sign(key, sum[:])
	require.NoError(t, err)
	ev.Sig = hex.EncodeToString(sig.Serialize())
	return ev
}

func TestRelayFloodIsBounded(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	var flood []nostrEvent
	for i := 0; i < maxRelayEvents+10; i++ {
		ev := signedEvent(t, key, nostrAnnounceKind, [][]string{{"d", strings.Repeat("a", 64)}}, strings.Repeat("x", i+1))
		flood = append(flood, ev)
	}
	relay := relayServing(t, flood...)
	allowPrivateAddresses = true
	t.Cleanup(func() { allowPrivateAddresses = false })
	events, err := queryRelay(relay, map[string]any{"kinds": []int{nostrAnnounceKind}})
	require.Error(t, err)
	assert.LessOrEqual(t, len(events), maxRelayEvents)
}
