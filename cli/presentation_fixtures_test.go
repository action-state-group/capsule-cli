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

// citations are the Capsule IDs a record cites: its chain parent and its
// agent-action-capsule references, the edges a bundle's closure follows.
func citations(record map[string]interface{}) []string {
	var ids []string
	if chain, ok := record["chain"].(map[string]interface{}); ok {
		if id, ok := chain["parent_capsule_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if references, ok := record["references"].([]interface{}); ok {
		for _, raw := range references {
			if ref, ok := raw.(map[string]interface{}); ok && ref["type"] == "agent-action-capsule" && ref["digest_alg"] == "SHA-256" {
				if id, ok := ref["digest"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// TestPresentationFixturesVerifyOffline holds every committed fixture to the
// verdict its meta.json records, from the bundle and from the page that
// carries it, with the core alone. The fixtures include deal copies, a
// seller's copy shared with the buyer among them: the core verifies a deal
// bundle without the deal plugin (capsule-cli#326, design note section 3.4).
func TestPresentationFixturesVerifyOffline(t *testing.T) {
	dirs, err := filepath.Glob("testdata/presentation/*/meta.json")
	require.NoError(t, err)
	require.NotEmpty(t, dirs)
	for _, metaPath := range dirs {
		dir := filepath.Dir(metaPath)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var meta struct {
				Verify string `json:"verify"`
			}
			raw, err := os.ReadFile(metaPath)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &meta))
			for _, file := range []string{"bundle.json", "page.html"} {
				path := filepath.Join(dir, file)
				if _, err := os.Stat(path); err != nil {
					continue
				}
				out, _ := invoke(t, "", "verify", "--bundle", path)
				var result struct {
					Verdict string `json:"verdict"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &result), out)
				assert.Equal(t, meta.Verify, result.Verdict, "verify --bundle %s", file)
			}
		})
	}
}
