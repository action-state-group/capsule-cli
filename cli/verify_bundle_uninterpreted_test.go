package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An extension kind the core does not read, a plugin's deal profile included
// (capsule-cli#428 step 5), is integrity-covered and listed uninterpreted. It
// adds no claim, so the verdict is the generic checks', and the line that says
// so names the kinds next to it (Sherlock2: VALID is what this verifier
// checked, never alone).
func TestVerifyBundleListsAnExtensionItDoesNotReadAndSaysSo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(v map[string]interface{}) {
		v["extensions"] = map[string]interface{}{
			"x-deal-v0":        map[string]interface{}{"sealed_report": "capsule-x"},
			"x-deal-v1":        map[string]interface{}{},
			"x-vendor-other/1": map[string]interface{}{},
		}
	})
	out, err := invoke(t, "", "verify", "--bundle", path)
	require.NoError(t, err, out)
	result := decodeResult(t, out)
	assert.Equal(t, "VALID", result["verdict"])
	kinds := map[string]string{}
	for _, raw := range result["extensions"].([]interface{}) {
		entry := raw.(map[string]interface{})
		kinds[entry["kind"].(string)] = entry["status"].(string)
	}
	assert.Equal(t, map[string]string{"x-deal-v0": "uninterpreted", "x-deal-v1": "uninterpreted", "x-vendor-other/1": "uninterpreted"}, kinds)
	assert.Equal(t, "Checked: unchanged since it was locked. Not checked here: x-deal-v0, x-deal-v1, x-vendor-other/1, which this tool does not interpret.", result["say"])
}

// A bundle with no extension the core skips keeps the plain line.
func TestVerifyBundleSaysNothingExtraWithoutUninterpretedExtensions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, err := invoke(t, "", "verify", "--bundle", producedBundle(t, nil))
	require.NoError(t, err, out)
	assert.Equal(t, "Checked: unchanged since it was locked.", decodeResult(t, out)["say"])
}

// The cadence chain (x-cadence-witness/v0) is plugin-local: the core reads no
// witness or as-of claim from it. The extension is listed uninterpreted, and
// the witness claim stays what it is for a bundle with no receipt, never a
// pass (Sherlock2, capsule-cli#428 step 5; the neutral form is
// parent-log-witness/v1, capsule-cli#608).
func TestVerifyBundleReadsNoWitnessClaimFromACadenceChain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(v map[string]interface{}) {
		v["extensions"] = map[string]interface{}{
			"x-cadence-witness/v0": map[string]interface{}{"state": "witnessed", "receipts": []interface{}{map[string]interface{}{"ts_url": testWitness, "receipt_b64": "AA=="}}},
		}
	})
	result, err := verifyBundleOutput(t, path)
	require.NoError(t, err)
	witnesses := result["witnesses"].(map[string]interface{})
	assert.NotEqual(t, "pass", witnesses["status"])
	assert.Empty(t, witnesses["receipts"])
	assert.Nil(t, result["registered_at"])
	ext := result["extensions"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "uninterpreted", ext["status"])
}

func decodeResult(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result
}
