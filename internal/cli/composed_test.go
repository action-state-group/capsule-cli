package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
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

// composedVectorsSHA256 pins testdata/composed-v1-vectors.json to
// agent-action-capsule vectors/bundle/composed/vectors.json at go/v0.7.0
// (its manifest records this digest). The copy is byte for byte.
const composedVectorsSHA256 = "223bc6a6b15fb9cb29231672458c8c4647ba488f77893cc685c499e72a6110c5"

type composedVector struct {
	ID        string                 `json:"id"`
	Container map[string]interface{} `json:"container"`
	Expect    map[string]interface{} `json:"expect"`
}

func composedVectors(t *testing.T) []composedVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "composed-v1-vectors.json"))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, composedVectorsSHA256, hex.EncodeToString(sum[:]))
	var file struct {
		Cases []composedVector `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &file))
	require.Len(t, file.Cases, 3)
	return file.Cases
}

func writeBundle(t *testing.T, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

func composedOf(t *testing.T, result map[string]interface{}) (string, map[string]interface{}) {
	t.Helper()
	extensions := result["extensions"].([]interface{})
	require.Len(t, extensions, 1)
	entry := extensions[0].(map[string]interface{})
	require.Equal(t, "composed/v1", entry["kind"])
	return entry["status"].(string), entry["composed"].(map[string]interface{})
}

func TestVerifyBundleComposedVectors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	wantStatus := map[string]string{"agree": "pass", "one-member-missing": "withheld", "same-custody-redundant": "pass"}
	for _, vector := range composedVectors(t) {
		t.Run(vector.ID, func(t *testing.T) {
			result, err := verifyBundleOutput(t, writeBundle(t, vector.Container))
			// The containing Bundle carries no checkpoint, so its interval
			// claims are not shown: INCOMPLETE, never VALID, in every case.
			assert.ErrorIs(t, err, ErrPartial)
			assert.Equal(t, "INCOMPLETE", result["verdict"])
			assert.Equal(t, "pass", status(result, "graph_closure"))

			extensionStatus, composed := composedOf(t, result)
			assert.Equal(t, wantStatus[vector.ID], extensionStatus)
			assert.Equal(t, extensionStatus, composed["status"])
			digest := composed["composed_digest"].(map[string]interface{})
			assert.Equal(t, vector.Expect["composed_digest"], digest["recomputed"])
			assert.Equal(t, true, digest["matches"])
			assert.NotEqual(t, result["bundle_digest"], digest["recomputed"])

			wantMembers := vector.Expect["members"].(map[string]interface{})
			members := composed["members"].([]interface{})
			require.Len(t, members, len(wantMembers))
			for _, raw := range members {
				member := raw.(map[string]interface{})
				want := wantMembers[member["id"].(string)].(map[string]interface{})
				for _, field := range []string{"outcome", "body", "digest"} {
					assert.Equal(t, want[field], member[field], field)
				}
				if want["claims"] == nil {
					assert.NotContains(t, member, "bundle")
					continue
				}
				bundle := member["bundle"].(map[string]interface{})
				for name, claimStatus := range want["claims"].(map[string]interface{}) {
					assert.Equal(t, claimStatus, status(bundle, name), name)
				}
				assert.Equal(t, "INCOMPLETE", bundle["verdict"])
				assert.Equal(t, "pass", status(bundle, "producer_signatures"))
			}

			wantClosure := vector.Expect["composition_closure"].(map[string]interface{})
			closure := composed["composition_closure"].(map[string]interface{})
			assert.Equal(t, wantClosure["status"], closure["status"])
			assert.Equal(t, wantClosure["missing"], closure["missing"])
			if finding, ok := wantClosure["finding"]; ok {
				assert.Equal(t, []interface{}{finding}, closure["findings"])
			}

			wantJoins := vector.Expect["joins"].([]interface{})
			joins := composed["joins"].([]interface{})
			require.Len(t, joins, len(wantJoins))
			for i, raw := range joins {
				join, want := raw.(map[string]interface{}), wantJoins[i].(map[string]interface{})
				for _, field := range []string{"members", "declared", "derived", "result"} {
					assert.Equal(t, want[field], join[field], field)
				}
			}

			wantPairs := vector.Expect["corroboration"].([]interface{})
			pairs := composed["corroboration"].([]interface{})
			require.Len(t, pairs, len(wantPairs))
			for i, raw := range pairs {
				pair := raw.(map[string]interface{})
				for field, value := range wantPairs[i].(map[string]interface{}) {
					assert.Equal(t, value, pair[field], field)
				}
			}
		})
	}
}

func TestVerifyBundleComposedTamperedMemberIsInvalid(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	container := composedVectors(t)[0].Container
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	member := block["members"].([]interface{})[0].(map[string]interface{})
	member["bundle"].(map[string]interface{})["verification"] = map[string]interface{}{"note": "added"}

	result, err := verifyBundleOutput(t, writeBundle(t, container))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "INVALID", result["verdict"])
	extensionStatus, composed := composedOf(t, result)
	assert.Equal(t, "fail", extensionStatus)
	assert.Equal(t, "mismatch", composed["members"].([]interface{})[0].(map[string]interface{})["digest"])
	assert.Equal(t, "fail", composed["composition_closure"].(map[string]interface{})["status"])
}

func TestVerifyBundleComposedMalformedReportsNoComposition(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	container := composedVectors(t)[0].Container
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	block["joins"].([]interface{})[0].(map[string]interface{})["members"] = []interface{}{"responder-b", "responder-a"}

	result, err := verifyBundleOutput(t, writeBundle(t, container))
	assert.ErrorIs(t, err, ErrBundleInvalid)
	_, composed := composedOf(t, result)
	assert.Equal(t, []interface{}{"composed_block_malformed:join:0:members_order"}, composed["findings"])
	for _, key := range []string{"composed_digest", "members", "composition_closure", "joins", "corroboration"} {
		assert.NotContains(t, composed, key)
	}
}

func signedRefusal(t *testing.T) (map[string]interface{}, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	refusal := map[string]interface{}{
		"request_digest": "9c785bd673591d47171b69dfde6924cc1b9cc27e26606b19125326b3ec0b9f12",
		"reason":         "policy_declined",
		"issued_at":      "2026-10-02T12:00:00Z",
		"key_id":         hex.EncodeToString(public),
	}
	message, err := canonical.JCS(refusal)
	require.NoError(t, err)
	refusal["signature"] = hex.EncodeToString(ed25519.Sign(private, message))
	return refusal, private
}

func TestVerifyRefusalSignature(t *testing.T) {
	refusal, _ := signedRefusal(t)
	assert.Equal(t, aacbundle.RefusalSignatureVerified, verifyRefusalSignature(refusal))

	refusal["reason"] = "not_authorized"
	assert.Equal(t, aacbundle.RefusalSignatureInvalid, verifyRefusalSignature(refusal))

	refusal, _ = signedRefusal(t)
	delete(refusal, "signature")
	assert.Equal(t, aacbundle.RefusalSignatureInvalid, verifyRefusalSignature(refusal))

	delete(refusal, "key_id")
	assert.Equal(t, aacbundle.RefusalSignatureUnverified, verifyRefusalSignature(refusal))
}

func TestVerifyBundleComposedRefusalMember(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	container := composedVectors(t)[0].Container
	block := container["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	b := block["members"].([]interface{})[1].(map[string]interface{})
	// Sign a refusal of responder-b's own request.
	refusal, private := signedRefusal(t)
	refusal["request_digest"] = b["request_digest"]
	delete(refusal, "signature")
	message, err := canonical.JCS(refusal)
	require.NoError(t, err)
	refusal["signature"] = hex.EncodeToString(ed25519.Sign(private, message))

	delete(b, "bundle")
	b["outcome"] = "refusal"
	b["refusal"] = refusal
	b["digest"], err = canonical.JSONDigest(refusal)
	require.NoError(t, err)
	block["joins"].([]interface{})[0].(map[string]interface{})["state"] = "one_sided"
	block["composed_digest"], err = aacbundle.ComposedDigest(block)
	require.NoError(t, err)

	result, err := verifyBundleOutput(t, writeBundle(t, container))
	assert.ErrorIs(t, err, ErrPartial)
	extensionStatus, composed := composedOf(t, result)
	assert.Equal(t, "pass", extensionStatus, composed["findings"])
	member := composed["members"].([]interface{})[1].(map[string]interface{})
	assert.Equal(t, "refusal", member["outcome"])
	assert.Equal(t, map[string]interface{}{"status": "verified", "profile": "ed25519-jcs"}, member["refusal_signature"])
	assert.NotContains(t, member, "bundle")
	join := composed["joins"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "one_sided", join["derived"])
	assert.Equal(t, "not_applicable", composed["corroboration"].([]interface{})[0].(map[string]interface{})["result"])
}

// The page gate refuses a composed/v1 bundle with the verdict verify --bundle
// gives it, member bundles included: a page can never claim more than verify
// --bundle does about a composition.
func TestPageGateAgreesWithVerifyOnComposedBundles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := map[string]map[string]interface{}{}
	for _, vector := range composedVectors(t) {
		cases[vector.ID] = vector.Container
	}
	tampered := composedVectors(t)[0].Container
	block := tampered["extensions"].(map[string]interface{})["composed/v1"].(map[string]interface{})
	block["members"].([]interface{})[0].(map[string]interface{})["bundle"].(map[string]interface{})["verification"] = map[string]interface{}{"note": "added"}
	cases["tampered-member"] = tampered

	for id, container := range cases {
		t.Run(id, func(t *testing.T) {
			path := writeBundle(t, container)
			result, verifyErr := verifyBundleOutput(t, path)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			value, err := decodeBundleJSON(raw)
			require.NoError(t, err)
			gate := pageGate(value)
			verdict := result["verdict"].(string)
			require.NotEqual(t, "VALID", verdict, "no composed vector carries a checkpoint")
			require.Error(t, gate)
			assert.Contains(t, gate.Error(), "calls this bundle "+verdict)
			assert.Equal(t, ExitCode(verifyErr), ExitCode(gate), "the gate exits as verify --bundle does")
		})
	}
	assert.Len(t, cases, 4)
}
