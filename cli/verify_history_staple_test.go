package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	raw     string
}

func runStaple(t *testing.T, path string, args ...string) stapleOut {
	t.Helper()
	out, _ := invoke(t, "", append([]string{"verify", "--history", path}, args...)...)
	var got stapleOut
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.NotEmpty(t, got.Staples, out)
	got.raw = out
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
	for _, name := range []string{"envelope_signature", "responder_trust", "card_form", "checkpoint_anchor", "checkpoint_signature", "commitment_signature", "stream_match", "commitment_anchor", "card_digest"} {
		assert.Equal(t, "PASS", p[name], name)
	}
	assert.Equal(t, "NOT_PRESENT", p["request_binding"])
	assert.Equal(t, "FAIL", p["subject_scope"], "the Goose node asks full_history, not the checkpoints subject (Sherlock2's finding)")
	assert.Equal(t, "NOT_CHECKED", p["checkpoint_key"], "no accepted log key was given")
	assert.NotContains(t, got.raw, "history_properties", "a history_card/1 has no History properties")

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
			got := runStaple(t, writeHistory(t, file), "--counterparty-key", v.Holder)
			row := got.Staples[0]
			assert.Equal(t, "PASS", row.Properties["responder_trust"])
			if v.Pass {
				for _, name := range []string{"envelope_signature", "request_binding", "card_form", "checkpoint_anchor", "checkpoint_signature"} {
					assert.Equal(t, "PASS", row.Properties[name], name)
				}
				return
			}
			assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			assert.NotEmpty(t, row.Findings, "a negative vector names what failed")
		})
	}
	assert.Positive(t, ran)
}

// With the sender's key and the log key both given, the served staple is
// ACCEPTED; with another log key it is not.
func TestVerifyHistoryStapledCardAcceptedWithBothKeys(t *testing.T) {
	var vectors []struct {
		Name    string `json:"name"`
		Holder  string `json:"holder"`
		Pass    bool   `json:"pass"`
		Request string `json:"request"`
		Reply   string `json:"reply"`
	}
	require.NoError(t, json.Unmarshal(checkedFile(t, "staple-v0/vectors.json", stapleVectorsSHA256), &vectors))
	for _, v := range vectors {
		if v.Name != "history_card/1" {
			continue
		}
		file, err := json.Marshal(map[string]any{"history": "history_card/1",
			"request": map[string]any{"media_type": "application/json", "octets_text": v.Request},
			"reply":   map[string]any{"media_type": "application/json", "octets_text": v.Reply}})
		require.NoError(t, err)
		path := writeHistory(t, file)
		first := runStaple(t, path, "--counterparty-key", v.Holder)
		key := first.Staples[0].Findings
		_ = key
		var logKey string
		for _, f := range runStaple(t, path, "--counterparty-key", v.Holder, "--checkpoint-key", gooseNodeKey).Staples[0].Findings {
			_, after, ok := cutAfter(f, "is signed by ")
			if ok {
				logKey = after[:64]
			}
		}
		require.NotEmpty(t, logKey, "the card's checkpoint names its signing key")
		ok := runStaple(t, path, "--counterparty-key", v.Holder, "--checkpoint-key", logKey)
		assert.Equal(t, "ACCEPTED", ok.Verdict, ok.raw)
		assert.Equal(t, "PASS", ok.Staples[0].Properties["checkpoint_key"])
		other := runStaple(t, path, "--counterparty-key", v.Holder, "--checkpoint-key", gooseNodeKey)
		assert.Equal(t, "NOT_ACCEPTED", other.Verdict)
		return
	}
	t.Fatal("no history_card/1 vector")
}

func cutAfter(s, sep string) (string, string, bool) { return strings.Cut(s, sep) }

// One property flipped at a time on the Goose card: each is a FAIL.
func TestVerifyHistoryStapledGooseCardTampered(t *testing.T) {
	raw := checkedFile(t, "goose-commitment-v1/commitment.json", gooseCommitmentSHA256)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	card := doc["history_cards"].([]any)[0].(map[string]any)
	envBytes, err := base64.StdEncoding.DecodeString(card["bytes"].(string))
	require.NoError(t, err)
	reseal := func(t *testing.T, mutate func(doc, card, env map[string]any)) string {
		t.Helper()
		var d map[string]any
		require.NoError(t, json.Unmarshal(raw, &d))
		c := d["history_cards"].([]any)[0].(map[string]any)
		var env map[string]any
		require.NoError(t, json.Unmarshal(envBytes, &env))
		mutate(d, c, env)
		out, err := json.Marshal(env)
		require.NoError(t, err)
		c["bytes"] = base64.StdEncoding.EncodeToString(out)
		file, err := json.Marshal(d)
		require.NoError(t, err)
		return writeHistory(t, file)
	}
	for name, tc := range map[string]struct {
		mutate   func(doc, card, env map[string]any)
		property string
	}{
		"a card digest that is not the card's": {func(_, c, _ map[string]any) { c["sha256"] = strings.Repeat("0", 64) }, "card_digest"},
		"a stream the card is not of":          {func(_, c, _ map[string]any) { c["stream"] = "goose/other" }, "stream_match"},
		"a commitment checkpoint that is another": {func(d, _, _ map[string]any) {
			d["checkpoint"].(map[string]any)["bytes"] = base64.StdEncoding.EncodeToString([]byte("x"))
		}, "commitment_anchor"},
		"an envelope whose artifact changed": {func(_, _, e map[string]any) { e["artifact"] = base64.StdEncoding.EncodeToString([]byte("{}")) }, "envelope_signature"},
	} {
		t.Run(name, func(t *testing.T) {
			got := runStaple(t, reseal(t, tc.mutate), "--counterparty-key", gooseNodeKey)
			assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			assert.Equal(t, "FAIL", got.Staples[0].Properties[tc.property], got.raw)
		})
	}
}

func TestVerifyHistoryStapleFlagsGoWithAStaple(t *testing.T) {
	path := filepath.Join("testdata", "goose-commitment-v1", "commitment.json")
	_, err := invoke(t, "", "verify", "--history", path, "--holder-key", gooseNodeKey)
	require.Error(t, err)
	c, hist := positiveHistory(t)
	_, err = invoke(t, "", "verify", "--history", hist, "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0], "--counterparty-key", gooseNodeKey)
	require.Error(t, err)
}
