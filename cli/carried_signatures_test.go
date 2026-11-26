package cli

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The refusal and response signature checks verify --bundle uses give AAC's
// carried signature profile vectors their expected state and profile,
// including the strict Ed25519 forgeries (a small-order key or an identity R)
// that plain Ed25519 accepts, and a three-member refusal that carries
// policy_digest, which is in no profile and stays unverified (capsule-cli#428).
func TestCarriedSignatureChecksMatchAACVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "carried-signature-profiles", "cases.json"))
	require.NoError(t, err)
	var doc struct {
		Cases []struct {
			ID     string          `json:"id"`
			Kind   string          `json:"kind"`
			Object json.RawMessage `json:"object"`
			Expect struct {
				Profile *string `json:"profile"`
				State   string  `json:"state"`
			} `json:"expect"`
			Lenient bool `json:"lenient_ed25519_verifies"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Len(t, doc.Cases, 43)
	forgeries := 0
	for _, c := range doc.Cases {
		t.Run(c.ID, func(t *testing.T) {
			// Decoded as verify --bundle decodes a bundle (exact numbers).
			object, err := DecodeBundleJSON(c.Object)
			require.NoError(t, err)
			var state, profile string
			if c.Kind == "refusal" {
				state, profile = verifyRefusalSignature(object), refusalProfile(object)
			} else {
				state, profile = verifyResponseSignature(object), responseProfile(object)
			}
			assert.Equal(t, c.Expect.State, state)
			want := ""
			if c.Expect.Profile != nil {
				want = *c.Expect.Profile
			}
			assert.Equal(t, want, profile)
			if c.Lenient {
				forgeries++
				assert.Equal(t, aacbundle.RefusalSignatureInvalid, state, "strict Ed25519 refuses what plain Ed25519 accepts")
			}
		})
	}
	assert.Equal(t, 6, forgeries, "the six strict forgeries are in the set")
}

// The forgery cases are real tests of strictness: plain Ed25519 accepts each.
func TestStrictForgeriesVerifyUnderPlainEd25519(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "carried-signature-profiles", "cases.json"))
	require.NoError(t, err)
	var doc struct {
		Cases []struct {
			ID      string                 `json:"id"`
			Kind    string                 `json:"kind"`
			Object  map[string]interface{} `json:"object"`
			Lenient bool                   `json:"lenient_ed25519_verifies"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	for _, c := range doc.Cases {
		if !c.Lenient {
			continue
		}
		var input []byte
		sigMember := "sig"
		switch {
		case c.Kind == "response":
			input, err = ArtifactResponseSigningBody(c.Object["request_digest"].(string), c.Object["anchor"].(string), c.Object["artifact_kind"].(string), c.Object["artifact_digest"].(string), c.Object["issued_at"].(string))
		case refusalProfile(c.Object) == ThreeMemberRefusalProfile:
			input, err = canonical.JCS(map[string]interface{}{"issued_at": c.Object["issued_at"], "reason": c.Object["reason"], "request_digest": c.Object["request_digest"]})
		default:
			sigMember = "signature"
			unsigned := map[string]interface{}{}
			for k, v := range c.Object {
				if k != "signature" {
					unsigned[k] = v
				}
			}
			input, err = canonical.JCS(unsigned)
		}
		require.NoError(t, err, c.ID)
		key, _ := hex.DecodeString(c.Object["key_id"].(string))
		sig, _ := hex.DecodeString(c.Object[sigMember].(string))
		assert.True(t, ed25519.Verify(ed25519.PublicKey(key), input, sig), "%s: plain Ed25519 accepts it", c.ID)
	}
}
