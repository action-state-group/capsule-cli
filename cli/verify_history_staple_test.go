package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vector files are byte copies: staples.json from evidencebook
// testdata/staple at d35127a, and a Goose node's evidence-commitment/1 as
// evidencebook's scripted run writes it (python/examples/goose/scripted_run.py
// at d35127a, #325), the node's own key being the commitment's key_id.
const (
	stapleVectorsSHA256   = "5520a8fae11d7ef43df426df6eaf348424cb0ad5975f4aca6454a06c0b4d531d"
	gooseCommitmentSHA256 = "956adc4840ef8937a7576fb714aa06e6c9c89ff134da3935ce1d8a232879eb80"
	gooseNodeKey          = "8c0c0699a2514ab17971f9596d05cd2ef121f4b21f5aadef34d42edb5a018088"
)

type stapleOut struct {
	Kind            string `json:"kind"`
	Verdict         string `json:"verdict"`
	AnchorWitnessed struct {
		Status string `json:"status"`
	} `json:"anchor_witnessed"`
	Staples []stapleRow `json:"staples"`
}

func runStaple(t *testing.T, path string, args ...string) stapleOut {
	t.Helper()
	out, _ := invoke(t, "", append([]string{"verify", "--history", path}, args...)...)
	var got stapleOut
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.NotEmpty(t, got.Staples, out)
	return got
}

func checkedFile(t *testing.T, name, want string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, want, hex.EncodeToString(sum[:]))
	return raw
}

// A Goose commitment staples the card as reply octets with no request, so
// it can never be ACCEPTED; every other property is read from the card.
func TestVerifyHistoryStapledGooseCommitment(t *testing.T) {
	checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
	path := filepath.Join("testdata", "goose-commitment-v1", "commitment.json")

	got := runStaple(t, path, "--counterparty-key", gooseNodeKey)
	assert.Equal(t, "history_card/1", got.Kind)
	assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
	assert.Equal(t, "NOT_PRESENT", got.AnchorWitnessed.Status)
	p := got.Staples[0].Properties
	for _, name := range []string{"envelope_signature", "responder_trust", "card_form", "anchor_match", "checkpoint_signature", "commitment_signature", "commitment_anchor", "card_digest"} {
		assert.Equal(t, "PASS", p[name], name)
	}
	assert.Equal(t, "NOT_PRESENT", p["request_binding"])
	assert.Equal(t, "NOT_CHECKED", p["checkpoint_key"], "no accepted log key was given")
	assert.NotContains(t, p, "history_properties", "a history_card/1 has no History properties")

	t.Run("no counterparty key checks nothing about the sender", func(t *testing.T) {
		p := runStaple(t, path).Staples[0].Properties
		assert.Equal(t, "NOT_CHECKED", p["responder_trust"])
		assert.Equal(t, "NOT_CHECKED", p["commitment_signature"])
	})
	// Attack F: a man in the middle re-signs everything under his own key,
	// so every internal check passes; only the key the reader holds differs.
	t.Run("another counterparty key fails", func(t *testing.T) {
		row := runStaple(t, path, "--counterparty-key", hex.EncodeToString(make([]byte, 32))).Staples[0]
		assert.Equal(t, "FAIL", row.Properties["responder_trust"])
		assert.Equal(t, "FAIL", row.Properties["commitment_signature"])
	})
	t.Run("the log key is the reader's, not the sender's", func(t *testing.T) {
		var doc struct {
			Cards []struct {
				Bytes string `json:"bytes"`
			} `json:"history_cards"`
		}
		require.NoError(t, json.Unmarshal(checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256), &doc))
		key := checkpointKeyOf(t, path)
		pass := runStaple(t, path, "--counterparty-key", gooseNodeKey, "--checkpoint-key", key).Staples[0]
		assert.Equal(t, "PASS", pass.Properties["checkpoint_key"])
		assert.NotEqual(t, gooseNodeKey, key, "the Goose node signs checkpoints with a separate key")
		fail := runStaple(t, path, "--counterparty-key", gooseNodeKey, "--checkpoint-key", gooseNodeKey).Staples[0]
		assert.Equal(t, "FAIL", fail.Properties["checkpoint_key"])
	})
	t.Run("a tampered commitment fails its signature", func(t *testing.T) {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256), &doc))
		doc["answers_within"] = "PT1H"
		tampered, err := json.Marshal(doc)
		require.NoError(t, err)
		row := runStaple(t, writeHistory(t, tampered), "--counterparty-key", gooseNodeKey).Staples[0]
		assert.Equal(t, "FAIL", row.Properties["commitment_signature"])
	})
}

// checkpointKeyOf is the key that signed the commitment's checkpoint.
func checkpointKeyOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc struct {
		Checkpoint struct {
			Bytes string `json:"bytes"`
		} `json:"checkpoint"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	statement, err := base64.StdEncoding.DecodeString(doc.Checkpoint.Bytes)
	require.NoError(t, err)
	signed, err := witness.ParseSignedCheckpoint(statement)
	require.NoError(t, err)
	return signed.KeyID
}

// Every history_card/1 case of evidencebook's staple vectors, through the
// same file shape a served staple has: the holder key is the reader's input.
func TestVerifyHistoryStapledCardRunsTheStapleVectors(t *testing.T) {
	var vectors []struct {
		Name    string `json:"name"`
		Holder  string `json:"holder"`
		Pass    bool   `json:"pass"`
		Request string `json:"request"`
		Reply   string `json:"reply"`
	}
	require.NoError(t, json.Unmarshal(checkedFile(t, "staple-v0/vectors.json", stapleVectorsSHA256), &vectors))
	ran := 0
	for _, v := range vectors {
		var req struct {
			Derivation string `json:"derivation"`
		}
		if json.Unmarshal([]byte(v.Request), &req) != nil || req.Derivation != "history_card/1" {
			continue
		}
		ran++
		t.Run(v.Name, func(t *testing.T) {
			file, err := json.Marshal(map[string]any{"history": "history_card/1",
				"request": map[string]any{"media_type": "application/json", "octets_text": v.Request},
				"reply":   map[string]any{"media_type": "application/json", "octets_text": v.Reply}})
			require.NoError(t, err)
			row := runStaple(t, writeHistory(t, file), "--counterparty-key", v.Holder).Staples[0]
			assert.Equal(t, "PASS", row.Properties["responder_trust"])
			if v.Pass {
				for _, name := range []string{"envelope_signature", "request_binding", "card_form", "anchor_match"} {
					assert.Equal(t, "PASS", row.Properties[name], name)
				}
			}
		})
	}
	assert.Positive(t, ran)
}
