package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	assert.Equal(t, "Checked: unchanged since it was locked. Not checked here: x-deal-v0, x-deal-v1, x-vendor-other/1, which this tool does not interpret; check these with the tool that defines them.", result["say"])
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

// The line names what was not read on a verdict that is not VALID too.
func TestVerifyNarrationNamesUninterpretedKindsOnEveryVerdict(t *testing.T) {
	result := map[string]any{"extensions": []any{map[string]any{"kind": "x-vendor/1", "status": "uninterpreted"}, map[string]any{"kind": "x-vendor/1", "status": "uninterpreted"}, map[string]any{"kind": "composed/v1", "status": "pass"}}}
	for _, err := range []error{ErrPartial, ErrBundleInvalid} {
		n := verifyNarration(&cobra.Command{}, result, err)
		assert.Contains(t, n.Say, "Not checked here: x-vendor/1, which this tool does not interpret", err.Error())
		assert.Equal(t, 1, strings.Count(n.Say, "x-vendor/1"), "each kind once")
	}
	assert.NotContains(t, verifyNarration(&cobra.Command{}, map[string]any{}, ErrPartial).Say, "Not checked here")
}

// An unread extension inside a composed member is named too: a valid
// composition must not hide it.
func TestUninterpretedKindsReachComposedMembers(t *testing.T) {
	result := map[string]any{
		"extensions": []any{map[string]any{"kind": "composed/v1", "status": "pass", "composed": map[string]any{
			"members": []any{
				map[string]any{"bundle": map[string]any{"extensions": []any{map[string]any{"kind": "x-vendor/2", "status": "uninterpreted"}}}},
				map[string]any{"bundle": map[string]any{"extensions": []any{map[string]any{"kind": "x-vendor/2", "status": "uninterpreted"}, map[string]any{"kind": "x-vendor/3", "status": "uninterpreted"}}}},
			}}}},
	}
	assert.Equal(t, []string{"x-vendor/2", "x-vendor/3"}, uninterpretedKinds(result))
}

// Only a bundle report's own extension entries count: a producer's body text
// that looks like one names no kind.
func TestUninterpretedKindsIgnoreProducerText(t *testing.T) {
	fake := map[string]any{"extensions": []any{map[string]any{"kind": "x-fake/1", "status": "uninterpreted"}}}
	result := map[string]any{
		"extensions": []any{map[string]any{"kind": "composed/v1", "status": "pass", "composed": map[string]any{
			"members": []any{map[string]any{"body": fake, "bundle": map[string]any{"extensions": []any{}}}}}}},
		"other": fake,
	}
	assert.Empty(t, uninterpretedKinds(result))
}
