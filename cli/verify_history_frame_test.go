package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/action-state-group/agent-action-capsule/go/witness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vector files are byte copies of agent-action-capsule vectors at
// a2182b58 (their SHA256SUMS).
const (
	anchorWitnessVectorsSHA256 = "21544e319dd46320f0c388b13080235fe0a258d0d8621bd1e77fbcf11253e59d"
	standardHistoryVectorsSHA  = "4126c66efaa5f5c94fa473432752f1eb596439a190476bd3799204afe3cb43c5"
)

func pinnedVectors(t *testing.T, dir, want string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", dir, "vectors.json"))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, want, hex.EncodeToString(sum[:]))
	return raw
}

// Every anchor-witness-v1 case, through the same call `verify --history`
// reports anchor_witnessed with.
func TestAnchorWitnessedRunsTheAnchorWitnessVectors(t *testing.T) {
	var file struct {
		Policies map[string]witness.AnchorWitnessPolicy `json:"policies"`
		Cases    []struct {
			Name     string          `json:"name"`
			Anchor   string          `json:"anchor"`
			Receipts []string        `json:"receipts"`
			Policy   json.RawMessage `json:"policy"`
			Expected struct {
				Status string `json:"status"`
			} `json:"expected"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(pinnedVectors(t, "anchor-witness-v1", anchorWitnessVectorsSHA256), &file))
	require.NotEmpty(t, file.Cases)
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			policy := file.Policies["default"]
			var named string
			if json.Unmarshal(tc.Policy, &named) != nil {
				require.NoError(t, json.Unmarshal(tc.Policy, &policy))
			}
			var receipts [][]byte
			for _, h := range tc.Receipts {
				b, err := hex.DecodeString(h)
				require.NoError(t, err)
				receipts = append(receipts, b)
			}
			got := anchorWitnessed(tc.Anchor, receipts, policy)
			assert.Equal(t, tc.Expected.Status, got.Status)
		})
	}
}

func TestVerifyHistoryNamesTheInstanceItCannotEvaluate(t *testing.T) {
	for id, reason := range map[string]string{
		"personal_history_basic/2": "unknown History instance",
		"made-up/9":                "unknown History instance",
	} {
		data, err := json.Marshal(map[string]any{"history": id})
		require.NoError(t, err)
		out, err := invoke(t, "", "verify", "--history", writeHistory(t, data))
		assert.ErrorIs(t, err, ErrHistoryNotAccepted, id)
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got), out)
		assert.Equal(t, "not_evaluable", got["verdict"], id)
		assert.Equal(t, reason, got["reason"], id)
		assert.Equal(t, id, got["kind"])
	}
}

func TestVerifyHistoryReportsAnchorWitnessedNotPresentWithoutAReceipt(t *testing.T) {
	c, path := positiveHistory(t)
	out, err := invoke(t, "", "verify", "--history", path, "--holder-key", c.Pins.TrustPolicy.ResponderKeys[0])
	require.NoError(t, err, out)
	var got struct {
		AnchorWitnessed struct {
			Status string `json:"status"`
		} `json:"anchor_witnessed"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "NOT_PRESENT", got.AnchorWitnessed.Status)
}

// signedStandardAnswer builds a served standard_history/1 answer for card,
// signed by key, as the file `verify --history` reads.
func signedStandardAnswer(t *testing.T, key ed25519.PrivateKey, card json.RawMessage, anchor string) []byte {
	t.Helper()
	request := `{"coverage":{"expected_pin":"` + anchor + `"},"derivation":"standard_history/1","nonce":"n-1","profile":"evidence-interop/0","subject":{"full_history":null}}`
	artifact, err := evidencerequest.EncodeJCS(mustDecode(t, card))
	require.NoError(t, err)
	requestSum := sha256.Sum256([]byte(request))
	artifactSum := sha256.Sum256(artifact)
	pub := key.Public().(ed25519.PublicKey)
	env := map[string]any{
		"profile": "evidence-interop/0", "request_digest": hex.EncodeToString(requestSum[:]),
		"subject": map[string]any{"full_history": nil}, "derivation": "standard_history/1",
		"anchor": map[string]any{"digest": anchor}, "artifact_digest": hex.EncodeToString(artifactSum[:]),
		"responder": hex.EncodeToString(pub), "issued_at": "2026-10-15T00:00:15Z",
	}
	body, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	env["sig"] = hex.EncodeToString(ed25519.Sign(key, body))
	env["artifact"] = base64.RawURLEncoding.EncodeToString(artifact)
	reply, err := evidencerequest.EncodeJCS(env)
	require.NoError(t, err)
	file, err := json.Marshal(map[string]any{
		"history": "standard_history/1",
		"request": map[string]any{"media_type": "application/json", "octets_text": request},
		"reply":   map[string]any{"media_type": "application/json", "octets_text": string(reply)},
	})
	require.NoError(t, err)
	return file
}

func mustDecode(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := evidencerequest.DecodeJSON(raw)
	require.NoError(t, err)
	return v
}

// A served standard_history/1 answer: its envelope is accepted, its card is
// a standard card, the properties that need the policy, the anchor time or
// the deal entries stay NOT_CHECKED, and a card with a count that has no
// denominator is not accepted.
func TestVerifyHistoryServedStandardHistory(t *testing.T) {
	var file struct {
		Cases []struct {
			ID     string `json:"id"`
			Inputs struct {
				Card json.RawMessage `json:"card"`
			} `json:"inputs"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(pinnedVectors(t, "standard-history-v0", standardHistoryVectorsSHA), &file))
	seed := sha256.Sum256([]byte("capsule-cli verify --history: served standard_history/1 test key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	holder := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	for _, id := range []string{"person-3-months", "count-without-denominator"} {
		for _, tc := range file.Cases {
			if tc.ID != id {
				continue
			}
			var card struct {
				Anchor string `json:"anchor"`
			}
			require.NoError(t, json.Unmarshal(tc.Inputs.Card, &card))
			path := writeHistory(t, signedStandardAnswer(t, key, tc.Inputs.Card, card.Anchor))
			out, err := invoke(t, "", "verify", "--history", path, "--holder-key", holder)
			var got struct {
				Verdict    string            `json:"verdict"`
				Properties map[string]string `json:"history_properties"`
				Envelope   map[string]string `json:"envelope_properties"`
				Anchor     struct {
					Status string `json:"status"`
				} `json:"anchor_witnessed"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), out)
			assert.Equal(t, "NOT_PRESENT", got.Anchor.Status, id)
			if id == "person-3-months" {
				require.NoError(t, err, out)
				assert.Equal(t, "ACCEPTED", got.Verdict)
				assert.Equal(t, "PASS", got.Properties["card_form"])
				assert.Equal(t, "NOT_CHECKED", got.Properties["counts_recompute"])
				assert.Equal(t, "PASS", got.Envelope["envelope_signature"])
			} else {
				assert.ErrorIs(t, err, ErrHistoryNotAccepted)
				assert.Equal(t, "NOT_ACCEPTED", got.Verdict)
			}
		}
	}
	// Under another key the envelope's responder_trust FAILs.
	tc := file.Cases[0]
	var card struct {
		Anchor string `json:"anchor"`
	}
	require.NoError(t, json.Unmarshal(tc.Inputs.Card, &card))
	path := writeHistory(t, signedStandardAnswer(t, key, tc.Inputs.Card, card.Anchor))
	_, err := invoke(t, "", "verify", "--history", path, "--holder-key", hex.EncodeToString(make([]byte, 32)))
	assert.ErrorIs(t, err, ErrHistoryNotAccepted)
}
