package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

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
				got := runStaple(t, path, "--counterparty-key", v.Holder)
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict, got.raw)
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
