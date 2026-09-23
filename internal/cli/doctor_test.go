package cli

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doctorReport(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, e := invoke(t, "", append([]string{"doctor"}, args...)...)
	require.NoError(t, e)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	return report
}

func TestDoctorWithoutProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	report := doctorReport(t)
	assert.Equal(t, cliVersion, report["binary_version"])
	profile, _ := report["profile"].(map[string]any)
	assert.Equal(t, "", profile["selected"])
	assert.Equal(t, false, profile["present"])
	assert.Equal(t, float64(0), profile["configured_profiles"])
	witness, _ := report["witness"].(map[string]any)
	assert.Equal(t, false, witness["checked"])
	plugins, _ := report["plugins"].(map[string]any)
	assert.NotNil(t, plugins["roots"])
}

func TestDoctorUnknownProfileReportsIssueNotFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	report := doctorReport(t, "--profile", "does-not-exist")
	profile, _ := report["profile"].(map[string]any)
	assert.Equal(t, false, profile["present"])
	assert.NotEmpty(t, profile["issue"])
}

func TestDoctorSigningKeyFilePermissions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	secretValue := p.Signing.Value
	keyPath := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(keyPath, []byte(secretValue), 0o600))
	p.Signing = Secret{File: keyPath}
	require.NoError(t, saveProfile(p, false))

	report := doctorReport(t, "--profile", p.Name)
	profile, _ := report["profile"].(map[string]any)
	assert.Equal(t, true, profile["present"])
	signing, _ := profile["signing_key"].(map[string]any)
	assert.Equal(t, "file", signing["source"])
	file, _ := signing["file"].(map[string]any)
	assert.Equal(t, true, file["ok"])
	assert.Equal(t, keyPath, file["path"])

	// Widen permissions: doctor must flag it, and the report never contains
	// the key material even though the file is now world-readable.
	require.NoError(t, os.Chmod(keyPath, 0o644))
	report = doctorReport(t, "--profile", p.Name)
	profile, _ = report["profile"].(map[string]any)
	signing, _ = profile["signing_key"].(map[string]any)
	file, _ = signing["file"].(map[string]any)
	assert.Equal(t, false, file["ok"])
	raw, e := json.Marshal(report)
	require.NoError(t, e)
	assert.NotContains(t, string(raw), secretValue)
}

func TestDoctorSigningKeyFromEnvNeverPrintsValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	secretValue := p.Signing.Value
	p.Signing = Secret{Env: "DOCTOR_TEST_SIGNING_KEY"}
	require.NoError(t, saveProfile(p, false))
	t.Setenv("DOCTOR_TEST_SIGNING_KEY", secretValue)

	report := doctorReport(t, "--profile", p.Name)
	profile, _ := report["profile"].(map[string]any)
	signing, _ := profile["signing_key"].(map[string]any)
	assert.Equal(t, "environment", signing["source"])
	assert.Equal(t, "DOCTOR_TEST_SIGNING_KEY", signing["env_var"])
	assert.Equal(t, true, signing["set"])
	raw, e := json.Marshal(report)
	require.NoError(t, e)
	assert.NotContains(t, string(raw), secretValue)
}

func TestDoctorWitnessReachabilityIsOptInAndNeverSendsAuth(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		assert.Equal(t, http.MethodHead, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, _ := profileFixture(t)
	p.Checkpoint.Endpoint = server.URL
	// serviceID (used elsewhere) requires HTTPS + a valid public key; doctor's
	// own reachability probe intentionally does not call serviceID, so an
	// http:// test server and an empty public key are fine here.
	require.NoError(t, saveProfile(p, false))

	// Without --check-witness: no request at all.
	report := doctorReport(t, "--profile", p.Name)
	witness, _ := report["witness"].(map[string]any)
	assert.Equal(t, false, witness["checked"])
	assert.False(t, sawAuth)

	// With --check-witness: an unauthenticated HEAD reaches the server.
	report = doctorReport(t, "--profile", p.Name, "--check-witness")
	witness, _ = report["witness"].(map[string]any)
	assert.Equal(t, true, witness["checked"])
	assert.Equal(t, true, witness["reachable"])
	request, _ := witness["request"].(map[string]any)
	assert.Equal(t, server.URL, request["url"])
	assert.False(t, sawAuth, "doctor must never attach a token to its reachability probe")
}

func TestDoctorPluginTrustWalk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	// group-writable: discovered but untrusted, so it must not appear.
	writeLauncher(t, root, "capsulectl-untrusted", fakePlugin, 0o775)

	report := doctorReport(t)
	plugins, _ := report["plugins"].(map[string]any)
	discovered, _ := plugins["discovered"].([]any)
	require.Len(t, discovered, 1)
	entry, _ := discovered[0].(map[string]any)
	assert.Equal(t, "guard", entry["name"])
}

func TestDoctorNeverPrintsAPrivateKeySeed(t *testing.T) {
	// A regression guard for the item's own "never prints a token" line: even
	// with an inline (Value) signing secret -- the least-safe of the three
	// sources -- doctor must not echo it.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	require.NoError(t, saveProfile(p, false))
	report := doctorReport(t, "--profile", p.Name)
	raw, e := json.Marshal(report)
	require.NoError(t, e)
	assert.NotContains(t, string(raw), p.Signing.Value)
	_, err := hex.DecodeString(p.Signing.Value)
	require.NoError(t, err, "sanity: fixture signing value is a real hex seed, not an already-safe placeholder")
}
