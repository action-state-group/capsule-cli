package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	aacbundle "github.com/action-state-group/agent-action-capsule/go/bundle"
	"github.com/action-state-group/agent-action-capsule/go/canonical"
	"github.com/action-state-group/agent-action-capsule/go/evidencerequest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ed25519Signer ed25519.PrivateKey

func (s ed25519Signer) Sign(_ context.Context, message []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(s), message), nil
}

// interopComposition is a composed/v1 bundle whose containing bundle and one
// artifact member are bundles this CLI produces over checkpointed logs, so
// both verify VALID. The member carries an evidence-interop envelope under
// profile, signed by the member's observer with the AAC evidencerequest
// signer, as received minus its artifact. edit changes the envelope's wire
// value before it is carried.
func interopComposition(t *testing.T, profile string, edit func(response map[string]interface{})) map[string]interface{} {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	responder := hex.EncodeToString(public)

	container, err := DecodeBundleJSON(mustRead(t, producedBundle(t, nil)))
	require.NoError(t, err)
	member, err := DecodeBundleJSON(mustRead(t, producedBundle(t, nil)))
	require.NoError(t, err)
	memberDigest, err := aacbundle.BundleDigest(member)
	require.NoError(t, err)
	cose, err := base64.RawURLEncoding.DecodeString(member["checkpoint"].(map[string]interface{})["cose"].(string))
	require.NoError(t, err)
	// The artifact octets are the JCS of the member bundle (it carries no
	// countersignatures), so artifact_digest is its BundleDigest (D4).
	require.NotContains(t, member, "countersignatures")
	artifact, err := canonical.JCS(member)
	require.NoError(t, err)
	require.Equal(t, memberDigest, evidencerequest.Digest(artifact))

	requestDigest := strings.Repeat("c", 64)
	envelope, err := evidencerequest.Envelope{
		Profile:             profile,
		RequestDigest:       requestDigest,
		Subject:             evidencerequest.Subject{Kind: evidencerequest.SubjectRecord, Digest: memberDigest},
		Anchor:              evidencerequest.Anchor{Digest: evidencerequest.Digest(cose)},
		ArtifactDigest:      memberDigest,
		Verification:        []evidencerequest.VerificationEntry{{Kind: "cll-checkpoint", Digest: evidencerequest.Digest(cose)}},
		Responder:           responder,
		IssuedAt:            "2026-10-31T00:00:00Z",
		Artifact:            artifact,
		VerificationContent: [][]byte{cose},
	}.Sign(t.Context(), ed25519Signer(private))
	require.NoError(t, err)
	require.NoError(t, envelope.Verify())
	response := carriedEnvelope(t, envelope)
	if edit != nil {
		edit(response)
	}

	block := map[string]interface{}{
		"members": []interface{}{map[string]interface{}{
			"id": "provider", "observer": responder, "request_digest": requestDigest,
			"outcome": "artifact", "digest": memberDigest, "bundle": member, "response": response,
		}},
		"observers": []interface{}{map[string]interface{}{"id": responder, "role": "party", "custody_domain": strings.Repeat("d", 64)}},
		"joins":     []interface{}{},
		// ComposedDigest reads a whole block, so a placeholder stands in
		// until it is computed.
		"composed_digest": strings.Repeat("0", 64),
	}
	block["composed_digest"], err = aacbundle.ComposedDigest(block)
	require.NoError(t, err)
	container["extensions"] = map[string]interface{}{"composed/v1": block}
	return container
}

func interopMember(t *testing.T, container map[string]interface{}) (map[string]interface{}, map[string]interface{}, error) {
	t.Helper()
	result, err := verifyBundleOutput(t, writeBundle(t, container))
	_, composed := composedOf(t, result)
	members := composed["members"].([]interface{})
	require.Len(t, members, 1)
	return result, members[0].(map[string]interface{}), err
}

func TestVerifyBundleComposedInteropEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, profile := range evidencerequest.Profiles {
		t.Run(profile, func(t *testing.T) {
			result, member, err := interopMember(t, interopComposition(t, profile, nil))
			require.NoError(t, err, member["findings"])
			assert.Equal(t, "VALID", result["verdict"])
			assert.Equal(t, map[string]interface{}{"status": "verified", "profile": InteropResponseProfile}, member["response_signature"])
			assert.Empty(t, member["findings"])
			assert.Equal(t, "VALID", member["bundle"].(map[string]interface{})["verdict"])
		})
	}
}

func TestVerifyBundleComposedInteropEnvelopeFailures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := []struct {
		name      string
		edit      func(map[string]interface{})
		signature string
		finding   string
	}{
		{"tampered signature", func(r map[string]interface{}) {
			sig := r["sig"].(string)
			swap := "0"
			if sig[0] == '0' {
				swap = "1"
			}
			r["sig"] = swap + sig[1:]
		}, "invalid", "response_signature_invalid"},
		{"signed member changed", func(r map[string]interface{}) { r["issued_at"] = "2026-10-31T00:00:01Z" }, "invalid", "response_signature_invalid"},
		{"profile not implemented", func(r map[string]interface{}) { r["profile"] = "evidence-interop/9" }, "invalid", "response_signature_invalid"},
		{"artifact carried", func(r map[string]interface{}) { r["artifact"] = "" }, "invalid", "response_signature_invalid"},
		{"key_id beside responder", func(r map[string]interface{}) { r["key_id"] = r["responder"] }, "invalid", "response_signature_invalid"},
		// The anchor is signed, so a wrong anchor that still verifies is one
		// the responder signed: re-signed below.
		{"anchor not the member checkpoint", nil, "verified", "response_anchor_not_checkpoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			container := interopComposition(t, evidencerequest.ProfileIDv1, c.edit)
			if c.edit == nil {
				resignWithAnchor(t, container, strings.Repeat("e", 64))
			}
			result, member, err := interopMember(t, container)
			assert.ErrorIs(t, err, ErrBundleInvalid)
			assert.Equal(t, "INVALID", result["verdict"])
			assert.Equal(t, c.signature, member["response_signature"].(map[string]interface{})["status"])
			assert.Contains(t, member["findings"], c.finding)
		})
	}
}

// A responder other than the member's observer fails, as key_id does for the
// deal form.
func TestVerifyBundleComposedInteropResponderNotObserver(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	container := interopComposition(t, evidencerequest.ProfileID, nil)
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	other := strings.Repeat("f", 64)
	block["members"].([]interface{})[0].(map[string]interface{})["observer"] = other
	block["observers"].([]interface{})[0].(map[string]interface{})["id"] = other
	var err error
	block["composed_digest"], err = aacbundle.ComposedDigest(block)
	require.NoError(t, err)
	result, member, err := interopMember(t, container)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "INVALID", result["verdict"])
	assert.Equal(t, "verified", member["response_signature"].(map[string]interface{})["status"])
	assert.Equal(t, []interface{}{"response_key_not_observer"}, member["findings"])
}

// resignWithAnchor replaces the carried envelope's anchor and signs it again
// under a fresh responder that the member then names as its observer.
func resignWithAnchor(t *testing.T, container map[string]interface{}, anchor string) {
	t.Helper()
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	member := block["members"].([]interface{})[0].(map[string]interface{})
	response := member["response"].(map[string]interface{})
	envelope, ok := interopEnvelope(response)
	require.True(t, ok)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	// The carried response has no artifact; put the member's back so the
	// re-signed envelope is whole.
	envelope.Artifact, err = canonical.JCS(member["bundle"])
	require.NoError(t, err)
	envelope.Anchor.Digest = anchor
	envelope.Responder = hex.EncodeToString(public)
	envelope, err = envelope.Sign(t.Context(), ed25519Signer(private))
	require.NoError(t, err)
	member["response"] = carriedEnvelope(t, envelope)
	member["observer"] = envelope.Responder
	block["observers"].([]interface{})[0].(map[string]interface{})["id"] = envelope.Responder
	block["composed_digest"], err = aacbundle.ComposedDigest(block)
	require.NoError(t, err)
}

// carriedEnvelope is envelope as a responder sends it in JSON (profile
// §11.4.1), read back as received and without its artifact, as a composed
// member carries it.
func carriedEnvelope(t *testing.T, envelope evidencerequest.Envelope) map[string]interface{} {
	t.Helper()
	encoded, err := evidencerequest.Encode(evidencerequest.MediaTypeJSON, envelope.Value())
	require.NoError(t, err)
	reply, err := evidencerequest.ParseReply(evidencerequest.Message{Octets: encoded, MediaType: evidencerequest.MediaTypeJSON})
	require.NoError(t, err)
	require.NoError(t, reply.Envelope.Verify())
	received, err := DecodeBundleJSON(encoded)
	require.NoError(t, err)
	delete(received, "artifact")
	return received
}
