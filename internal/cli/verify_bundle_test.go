package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// producedBundle is an Evidence Bundle this CLI's own producer assembles
// over a checkpointed log, written to a file.
func producedBundle(t *testing.T, edit func(map[string]interface{})) string {
	t.Helper()
	profile, key := profileFixture(t)
	store, log, root := checkpointedStore(t, key, profile.LogID)
	bundle, err := AssembleBundle(t.Context(), store, log, profile.LogID, BundleOptions{Root: root, ClosureDepth: 2, Payloads: "none"})
	require.NoError(t, err)
	encoded, err := json.Marshal(bundle)
	require.NoError(t, err)
	value, err := decodeBundleJSON(encoded)
	require.NoError(t, err)
	if edit != nil {
		edit(value)
	}
	out, err := json.Marshal(value)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, out, 0o600))
	return path
}

func verifyBundleOutput(t *testing.T, path string) (map[string]interface{}, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--bundle", path)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &result), out)
	return result, err
}

func status(result map[string]interface{}, claim string) string {
	return result[claim].(map[string]interface{})["status"].(string)
}

func TestVerifyBundleAcceptsAProducedBundleOffline(t *testing.T) {
	// No profile is selected or needed: the file alone is verified.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	result, err := verifyBundleOutput(t, producedBundle(t, nil))
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	for _, claim := range []string{"graph_closure", "interval_coverage", "per_record_membership"} {
		assert.Equal(t, "pass", status(result, claim), claim)
	}
	assert.NotEmpty(t, result["bundle_digest"])
}

func TestVerifyBundleWithoutACheckpointSignatureIsIncomplete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		delete(b["checkpoint"].(map[string]interface{}), "cose")
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, 3, ExitCode(err))
	assert.Equal(t, "INCOMPLETE", result["verdict"])
	assert.Contains(t, result["interval_coverage"].(map[string]interface{})["findings"], "checkpoint_unverified")
}

func TestVerifyBundleRejectsAnEditedRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		b["records"].([]interface{})[0].(map[string]interface{})["operator"] = "someone-else"
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "INVALID", result["verdict"])
}

func TestVerifyBundleRejectsAForgedCheckpointSignature(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := producedBundle(t, func(b map[string]interface{}) {
		checkpoint := b["checkpoint"].(map[string]interface{})
		cose := checkpoint["cose"].(string)
		swap := "A"
		if cose[len(cose)-2:len(cose)-1] == "A" {
			swap = "B"
		}
		checkpoint["cose"] = cose[:len(cose)-2] + swap + cose[len(cose)-1:]
	})
	result, err := verifyBundleOutput(t, path)
	assert.ErrorIs(t, err, ErrBundleInvalid)
	assert.Equal(t, "fail", status(result, "interval_coverage"))
}

func TestVerifyBundleRefusesAFileThatIsNotABundle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "x.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schema":"some-pane-export/1"}`), 0o600))
	_, err := invoke(t, "", "verify", "--bundle", path)
	assert.Equal(t, 2, ExitCode(err))
}
