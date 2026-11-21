package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyCapsuleDealExportNamesTheReceipt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const wantA, wantB = "deal report --deal ID --html receipt.html", "verify --bundle receipt.html"
	for name, body := range map[string]string{
		"array":  `[{"body":{"a":1},"x-deal-v0":{"at":"t"}},{"body":{},"x-deal-v0":{}}]`,
		"object": `{"body":{"a":1},"x-deal-v0":{"at":"t"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rec.json")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			err := dealExportHint(path, []byte(body))
			require.ErrorIs(t, err, ErrInput)
			assert.Equal(t, 2, ExitCode(err))
			assert.Contains(t, SafeError(err), wantA)
			assert.Contains(t, SafeError(err), wantB)
		})
	}
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
