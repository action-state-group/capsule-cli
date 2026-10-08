package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bareCapsuleFixture = "testdata/capsule-emit/bare-capsule-0.8.6.json"

// bareCapsuleProfile saves a profile that trusts the given keys and returns
// the fixture's own key.
func bareCapsuleProfile(t *testing.T, trusted ...string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	raw, err := os.ReadFile(bareCapsuleFixture)
	require.NoError(t, err)
	var capsule map[string]any
	require.NoError(t, json.Unmarshal(raw, &capsule))
	p, _ := profileFixture(t)
	p.TrustedKeys = trusted
	require.NoError(t, saveProfile(p, false))
	return capsule["key_id"].(string)
}

func verifyCapsuleOutput(t *testing.T, path string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "verify", "--profile", "test", "--capsule", path)
	var result map[string]any
	if json.Unmarshal([]byte(out), &result) != nil {
		result = map[string]any{"raw": out}
	}
	return result, err
}

// A bare capsule, as capsule-emit seals it, verifies with the same checks as
// the artifact.Record wrapper: its capsule_id recomputed, its inline producer
// signature checked under its key_id, and that key held to the profile's
// trusted keys. The output says which shape it read.
func TestVerifyCapsuleReadsABareCapsule(t *testing.T) {
	key := bareCapsuleProfile(t)
	p, err := loadProfile("test")
	require.NoError(t, err)
	p.TrustedKeys = []string{key}
	require.NoError(t, saveProfile(p, true))

	result, err := verifyCapsuleOutput(t, bareCapsuleFixture)
	require.NoError(t, err, result)
	assert.Equal(t, "capsule", result["shape"])
	assert.Equal(t, "passed", result["capsule_identity"])
	assert.Equal(t, "passed", result["producer_signature_and_trust"])
	assert.Equal(t, key, result["key_id"])
	assert.Equal(t, "not_performed", result["cll_inclusion"])
	assert.Contains(t, result["originals"], "not carried")
}

// A bare capsule changed after sealing fails: an edited field no longer
// has its capsule_id, and an edited signature does not verify.
func TestVerifyCapsuleFailsATamperedBareCapsule(t *testing.T) {
	key := bareCapsuleProfile(t)
	p, err := loadProfile("test")
	require.NoError(t, err)
	p.TrustedKeys = []string{key}
	require.NoError(t, saveProfile(p, true))
	raw, err := os.ReadFile(bareCapsuleFixture)
	require.NoError(t, err)

	because := map[string][2]string{
		"an edited field":     {"capsule_identity", "capsule_id_mismatch"},
		"an edited signature": {"producer_signature", "does not verify"},
		"another key_id":      {"producer_signature", "does not verify"},
	}
	for name, edit := range map[string]func(map[string]any){
		"an edited field": func(c map[string]any) { c["operator"] = "someone-else" },
		"an edited signature": func(c map[string]any) {
			sig := []byte(c["signature"].(string))
			if sig[len(sig)-1] == '0' {
				sig[len(sig)-1] = '1'
			} else {
				sig[len(sig)-1] = '0'
			}
			c["signature"] = string(sig)
		},
		"another key_id": func(c map[string]any) { c["key_id"] = "00" + c["key_id"].(string)[2:] },
	} {
		var capsule map[string]any
		require.NoError(t, json.Unmarshal(raw, &capsule))
		edit(capsule)
		edited, err := json.Marshal(capsule)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "tampered.json")
		require.NoError(t, os.WriteFile(path, edited, 0o600))
		result, err := verifyCapsuleOutput(t, path)
		require.Error(t, err, name)
		assert.Equal(t, 1, ExitCode(err), name)
		assert.Equal(t, "capsule", result["shape"], name)
		assert.Contains(t, result[because[name][0]], because[name][1], name)
	}
}

// A key the profile does not trust fails, as for the wrapper; a profile with
// no trusted key checks identity and signature but not trust, and says so.
func TestVerifyCapsuleHoldsABareCapsuleToTheTrustedKeys(t *testing.T) {
	bareCapsuleProfile(t, strings.Repeat("11", 32))
	result, err := verifyCapsuleOutput(t, bareCapsuleFixture)
	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Equal(t, "passed", result["capsule_identity"])
	assert.Contains(t, result["producer_signature_and_trust"], "not among the profile's trusted keys")

	bareCapsuleProfile(t)
	result, err = verifyCapsuleOutput(t, bareCapsuleFixture)
	require.ErrorIs(t, err, ErrPartial)
	assert.Equal(t, "passed", result["capsule_identity"])
	assert.Equal(t, "passed", result["producer_signature"])
	assert.Equal(t, "not_performed: no trusted key", result["producer_trust"])
}

// The shape is read from the file, never guessed: a file that is neither an
// artifact.Record nor a bare capsule, or that looks like both, is refused
// with both expected shapes named. The wrapper's own path is unchanged and
// now names its shape.
func TestVerifyCapsuleNamesTheShapeItRead(t *testing.T) {
	bareCapsuleProfile(t)
	for name, body := range map[string]string{
		"neither":   `{"hello":"world"}`,
		"both":      `{"capsule_id":"x","capsule":"eA==","producer_envelope":"eA==","artifacts":[],"spec_version":"x","format_version":"x"}`,
		"not JSON":  `[1,2]`,
		"no fields": `{}`,
	} {
		path := filepath.Join(t.TempDir(), "x.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := invoke(t, "", "verify", "--profile", "test", "--capsule", path)
		require.ErrorIs(t, err, ErrInput, name)
		assert.Contains(t, err.Error(), "artifact.Record", name)
		assert.Contains(t, err.Error(), "bare Agent Action Capsule", name)
	}
}

func TestVerifyCapsuleNamesTheWrapperShape(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	dir := t.TempDir()
	request := filepath.Join(dir, "request.json")
	recordPath := filepath.Join(dir, "capsule.json")
	require.NoError(t, os.WriteFile(request, requestFixture(t), 0o600))
	_, err := invoke(t, "", "seal", "--profile", p.Name, "--request", request, "--output", recordPath)
	require.NoError(t, err)
	result, err := verifyCapsuleOutput(t, recordPath)
	require.NoError(t, err)
	assert.Equal(t, "artifact-record", result["shape"])
	assert.Equal(t, "passed", result["capsule_identity"])
}
