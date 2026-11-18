package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staples.json is a byte copy of evidencebook v0.16.0 testdata/history-card-2
// (the same bytes in v0.17.0): the history_card/2 staple vectors.
const card2VectorsSHA256 = "ea38f046c94000a8d54cf92090e9d48b5df0d2a415054362695d44867f5a2fbf"

type card2Vector struct {
	Name    string `json:"name"`
	Holder  string `json:"holder"`
	Pass    bool   `json:"pass"`
	Request string `json:"request"`
	Reply   string `json:"reply"`
}

func card2Vectors(t *testing.T) []card2Vector {
	t.Helper()
	var vectors []card2Vector
	require.NoError(t, json.Unmarshal(checkedFile(t, "history-card-2/staples.json", card2VectorsSHA256), &vectors))
	require.NotEmpty(t, vectors)
	return vectors
}

func card2File(t *testing.T, v card2Vector) string {
	t.Helper()
	file, err := json.Marshal(map[string]any{"history": "history_card/2",
		"request": map[string]any{"media_type": "application/json", "octets_text": v.Request},
		"reply":   map[string]any{"media_type": "application/json", "octets_text": v.Reply}})
	require.NoError(t, err)
	return writeHistory(t, file)
}

// Every vector of evidencebook's history_card/2 staple set: a passing one is
// ACCEPTED with the sender's key and the log key, a failing one never is.
func TestVerifyHistoryStapledCard2RunsTheVectors(t *testing.T) {
	for _, v := range card2Vectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			path := card2File(t, v)
			if !v.Pass {
				// With the log key the card names, so the refusal is the card's
				// own defect and not the missing key.
				args := []string{"--counterparty-key", v.Holder}
				if key := card2LogKey(v.Reply); key != "" {
					args = append(args, "--checkpoint-key", key)
				}
				got := runStaple(t, path, args...)
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict, got.raw)
				assert.Contains(t, got.raw, "FAIL", "the vector is refused for a failed check, not a missing one")
				return
			}
			got := runStaple(t, path, "--counterparty-key", v.Holder)
			assert.Equal(t, "history_card/2", got.Kind)
			row := got.Staples[0]
			for _, name := range []string{"envelope_signature", "responder_trust", "request_binding", "card_form", "subject_scope", "checkpoint_anchor", "checkpoint_signature", "checkpoint_consistency", "checkpoint_receipts"} {
				assert.Equal(t, "PASS", row.Properties[name], name)
			}
			assert.Equal(t, "NOT_CHECKED", row.Properties["checkpoint_key"], "no accepted log key was given")
			key := lastCheckpointKey(t, v.Reply)
			ok := runStaple(t, path, "--counterparty-key", v.Holder, "--checkpoint-key", key)
			assert.Equal(t, "ACCEPTED", ok.Verdict, ok.raw)
			other := runStaple(t, path, "--counterparty-key", v.Holder, "--checkpoint-key", gooseNodeKey)
			assert.Equal(t, "NOT_ACCEPTED", other.Verdict)
			assert.Equal(t, "FAIL", other.Staples[0].Properties["checkpoint_key"])
		})
	}
}

// card2LogKey is the key the card's last checkpoint names, or "" when the
// card does not read that far.
func card2LogKey(reply string) string {
	rep, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: []byte(reply), MediaType: evidencerequest.MediaTypeJSON})
	if err != nil || rep.Envelope == nil {
		return ""
	}
	decoded, err := evidencerequest.DecodeJSON(rep.Envelope.Artifact)
	card, _ := decoded.(map[string]any)
	history, _ := card["history"].([]any)
	if err != nil || len(history) == 0 {
		return ""
	}
	entry, _ := history[len(history)-1].(map[string]any)
	hexed, _ := entry["checkpoint"].(string)
	statement, err := hex.DecodeString(hexed)
	if err != nil {
		return ""
	}
	signed, err := witness.ParseSignedCheckpoint(statement)
	if err != nil {
		return ""
	}
	return signed.KeyID
}

// A commitment that staples only a history_card/2 is read as a card, not
// reported as stapling none (the /1-only reader's message).
func TestVerifyHistoryCommitmentStaplingCard2(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "history_card/2" {
			continue
		}
		sum := sha256.Sum256([]byte(v.Reply))
		doc, err := json.Marshal(map[string]any{"profile": "evidence-commitment/1",
			"history_cards": []any{map[string]any{"stream": "book-sh-log", "derivation": "history_card/2",
				"bytes": base64.StdEncoding.EncodeToString([]byte(v.Reply)), "sha256": hex.EncodeToString(sum[:])}}})
		require.NoError(t, err)
		got := runStaple(t, writeHistory(t, doc), "--counterparty-key", v.Holder, "--checkpoint-key", lastCheckpointKey(t, v.Reply))
		row := got.Staples[0]
		for _, name := range []string{"envelope_signature", "responder_trust", "card_form", "checkpoint_anchor", "checkpoint_signature", "checkpoint_consistency", "checkpoint_key", "card_digest"} {
			assert.Equal(t, "PASS", row.Properties[name], name+": "+got.raw)
		}
		return
	}
	t.Fatal("no history_card/2 vector")
}

// A /2 card is not a /1 card: the envelope's derivation names the rules.
func TestVerifyHistoryCard2KeepsCard1Apart(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "a card of the history_card/1 derivation" {
			continue
		}
		got := runStaple(t, card2File(t, v), "--counterparty-key", v.Holder)
		assert.Equal(t, "NOT_ACCEPTED", got.Verdict, got.raw)
	}
}

// A bounded card samples the log, so a chain need not contain the checkpoint
// a reader holds; contains is what the live check asks before extendsPin.
func TestCardChainContains(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "history_card/2" {
			continue
		}
		chain, ok := chainOf([]byte(v.Reply))
		require.True(t, ok)
		assert.Equal(t, stapleCard2, chain.Derivation)
		last := chain.Parsed[len(chain.Parsed)-1]
		assert.True(t, chain.contains(last.MMRSize))
		assert.False(t, chain.contains(last.MMRSize+1))
		return
	}
	t.Fatal("no history_card/2 vector")
}

// --also-served takes a /2 answer: two answers to one /2 question compare.
func TestVerifyHistoryAlsoServedTakesCard2(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "history_card/2" {
			continue
		}
		path := card2File(t, v)
		got := runStaple(t, path, "--counterparty-key", v.Holder, "--also-served", path)
		assert.Equal(t, "NOT_CHECKED", got.Staples[0].Properties["caller_invariance"], got.raw)
		return
	}
	t.Fatal("no history_card/2 vector")
}

// A sampled chain past the held size, without it, is neither extended nor
// contradicted; a chain that ends before it or holds it is judged as before.
func TestSampledPast(t *testing.T) {
	for _, v := range card2Vectors(t) {
		if v.Name != "history_card/2" {
			continue
		}
		chain, ok := chainOf([]byte(v.Reply))
		require.True(t, ok)
		last := chain.Parsed[len(chain.Parsed)-1].MMRSize
		assert.False(t, chain.sampledPast(last), "the card holds that size")
		assert.False(t, chain.sampledPast(last+1), "a log shorter than the held size is not sampled past it")
		var missing uint64
		for size := uint64(1); size < last; size++ {
			if !chain.contains(size) {
				missing = size
				break
			}
		}
		require.NotZero(t, missing, "the card samples its log")
		assert.True(t, chain.sampledPast(missing))
		chain.Derivation = stapleCard
		assert.False(t, chain.sampledPast(missing), "a /1 card lists every checkpoint")
		return
	}
	t.Fatal("no history_card/2 vector")
}

// `commitment find` reads a /2 staple like a /1 one: the card's rows come from
// the same checks, and a stream pinned earlier that a sampled chain no longer
// lists is a note, not a pin failure.
func TestFindReadsACard2Staple(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var reply string
	for _, v := range card2Vectors(t) {
		if v.Name == "history_card/2" {
			reply = v.Reply
		}
	}
	require.NotEmpty(t, reply)
	octets := resignWith(t, key, func(doc map[string]any) {
		card := doc["history_cards"].([]any)[0].(map[string]any)
		card["bytes"], card["derivation"], card["stream"] = base64.StdEncoding.EncodeToString([]byte(reply)), "history_card/2", "book-sh-log"
		restaple(t, doc, key)
	})
	path := writeHistory(t, octets)
	nodeKey := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	rep, _ := find(t, "--file", path, "--no-pin", "--offline", "--node-key", nodeKey)
	require.NotEmpty(t, rep.Nodes)
	require.NotEmpty(t, rep.Nodes[0].Staples, "the /2 card is read, not reported as stapling none")
	p := rep.Nodes[0].Staples[0].Properties
	for _, name := range []string{"envelope_signature", "card_form", "checkpoint_signature", "checkpoint_consistency", "checkpoint_anchor", "card_digest"} {
		assert.Equal(t, "PASS", p[name], name)
	}
	assert.NotContains(t, rep.Nodes[0].Reasons, "staples")

	chain, ok := chainOf([]byte(reply))
	require.True(t, ok)
	last := chain.Parsed[len(chain.Parsed)-1]
	pins := &commitmentPins{Streams: map[string]streamPin{chain.Stream: {LogID: last.LogID, Size: 3, Root: last.Root}}}
	require.True(t, chain.sampledPast(3), "a pin at a size the card samples out")
	var failures []string
	notes := pinChecks(pins, []cardChain{chain}, time.Now(), false, func(reason, finding string) { failures = append(failures, reason) })
	assert.Empty(t, failures)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "not checked")
}
