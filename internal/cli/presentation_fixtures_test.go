package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPresentationGoldensCiteWhatTheyCarry holds every presentation golden's
// citations to what a closure walks: a capsule reference reads SHA-256 (any
// other spelling is never followed, so the bundle verifies without it), and
// each record a reference names is in the bundle (capsule-cli#352).
func TestPresentationGoldensCiteWhatTheyCarry(t *testing.T) {
	bundles, err := filepath.Glob("testdata/presentation/*/bundle.json")
	require.NoError(t, err)
	require.NotEmpty(t, bundles)
	cited := 0
	for _, path := range bundles {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			var bundle struct {
				Records []map[string]interface{} `json:"records"`
			}
			require.NoError(t, json.Unmarshal(raw, &bundle))
			carried := map[string]bool{}
			for _, r := range bundle.Records {
				if id, ok := r["capsule_id"].(string); ok {
					carried[id] = true
				}
			}
			for _, r := range bundle.Records {
				refs, _ := r["references"].([]interface{})
				for _, raw := range refs {
					ref, ok := raw.(map[string]interface{})
					if !ok || ref["type"] != "agent-action-capsule" {
						continue
					}
					assert.Equal(t, "SHA-256", ref["digest_alg"], "%s cites %v", r["capsule_id"], ref["digest"])
				}
				for _, id := range citations(r) {
					cited++
					assert.True(t, carried[id], "%s cites %s, which the bundle does not carry", r["capsule_id"], id)
				}
			}
		})
	}
	assert.Positive(t, cited, "some golden cites a record")
}
