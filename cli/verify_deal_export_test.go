package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyCapsuleDealExportNamesTheReceipt(t *testing.T) {
	newDisclosureProfile(t)
	for name, body := range map[string]string{
		"array":  `[{"body":{"a":1},"x-deal-v0":{"at":"t"}},{"body":{},"x-deal-v0":{}}]`,
		"object": `{"body":{"a":1},"x-deal-v0":{"at":"t"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rec.json")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			_, err := invoke(t, "", "verify", "--profile", "rep", "--capsule", path)
			require.ErrorIs(t, err, ErrInput)
			assert.Equal(t, 2, ExitCode(err))
			assert.Contains(t, SafeError(err), "deal export")
			assert.Contains(t, SafeError(err), "deal report --deal ID --html receipt.html")
			assert.Contains(t, SafeError(err), "verify --bundle receipt.html")
		})
	}
	// Any other array keeps the old message.
	path := filepath.Join(t.TempDir(), "other.json")
	require.NoError(t, os.WriteFile(path, []byte(`[1]`), 0o600))
	_, err := invoke(t, "", "verify", "--profile", "rep", "--capsule", path)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "is not a JSON object")
}

// A get miss on a profile whose sqlite store the deal plugin owns names the
// cause and exits 2; on a core-owned store it keeps the generic error's exit.
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
	assert.Contains(t, SafeError(err), "verify --bundle receipt.html")
}

func TestDealExportHintLeavesOtherInputAlone(t *testing.T) {
	for name, body := range map[string]string{
		"number array":   `[1]`,
		"empty array":    `[]`,
		"other objects":  `[{"a":1}]`,
		"mixed":          `[{"x-deal-v0":{}},{"a":1}]`,
		"null element":   `[null]`,
		"has capsule_id": `{"capsule_id":"ab","x-deal-v0":{}}`,
		"record":         `{"capsule_id":"ab","capsule":"","producer_envelope":""}`,
		"not json":       `hello`,
		"null":           `null`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, dealExportHint("f.json", []byte(body)))
		})
	}
	// Non-object input keeps the existing message.
	_, err := capsuleFileShape("f.json", []byte(`[1]`))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "is not a JSON object")
}

func TestOwnedStoreMissHint(t *testing.T) {
	deal := ownedStoreMissHint(Profile{Type: "sqlite", Namespace: "n", Log: "deal"}, "abc")
	assert.Contains(t, deal, "kept by the deal plugin")
	assert.Contains(t, deal, "verify --bundle receipt.html")
	other := ownedStoreMissHint(Profile{Type: "sqlite", Namespace: "n", Log: "book"}, "abc")
	assert.Contains(t, other, "kept by the book plugin")
	assert.NotContains(t, other, "deal report")
}
