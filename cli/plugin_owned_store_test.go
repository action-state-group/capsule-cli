package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A deal export (a JSON array of records with no capsule ID) gets the same
// answer as any other file that is not a capsule: the core keeps no special
// message for one plugin's export (capsule-cli#428 step 5).
func TestVerifyCapsuleRefusesAnArrayLikeAnyOtherNonCapsule(t *testing.T) {
	newDisclosureProfile(t)
	for name, body := range map[string]string{"array of records": `[{"body":{"a":1},"x-deal-v0":{"at":"t"}}]`, "number array": `[1]`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rec.json")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			_, err := invoke(t, "", "verify", "--profile", "rep", "--capsule", path)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), "is not a JSON object")
			assert.NotContains(t, SafeError(err), "deal export")
		})
	}
}

// A get miss on a profile whose sqlite store a plugin owns names the cause and
// exits 2; on a core-owned store it keeps the generic error's exit.
func TestGetMissOnPluginOwnedStore(t *testing.T) {
	newDisclosureProfile(t)
	missing := strings.Repeat("ab", 32)
	_, err := invoke(t, "", "get", "--profile", "rep", "--capsule-id", missing)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInput, "a core-owned store's miss is unchanged")

	p, err := LoadProfile("rep")
	require.NoError(t, err)
	p.Namespace, p.Log = "ns", "deal"
	require.NoError(t, SaveProfile(p, true))
	_, err = invoke(t, "", "get", "--profile", "rep", "--capsule-id", missing)
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 2, ExitCode(err))
	assert.Contains(t, SafeError(err), "kept by the deal plugin")
	assert.Contains(t, SafeError(err), "capsulectl deal --help")
}

func TestOwnedStoreMissHint(t *testing.T) {
	// The hint names the profile's declared owner and no owner's verbs.
	for _, owner := range []string{"deal", "book", "other"} {
		hint := ownedStoreMissHint(Profile{Type: "sqlite", Namespace: "n", Log: owner}, "abc")
		assert.Contains(t, hint, "kept by the "+owner+" plugin")
		assert.Contains(t, hint, "capsulectl "+owner+" --help")
		assert.NotContains(t, hint, "deal report")
		assert.NotContains(t, hint, "x-deal-v0")
	}
}
