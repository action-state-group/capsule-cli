package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A deal bundle's completeness claim covers every step of the deal, not the
// last few: its root is the latest step, and it states closure_depth (the
// citation hops walked from the root) as the number of steps before it, so a
// verifier never falls back to the Evidence Bundle default of 2. Here a
// 16-step buy-then-cancel deal, in the user's own copy and a shared one.
func TestDealBundleClaimsClosureOverEveryStep(t *testing.T) {
	id := cancelAfterPurchase(t)
	for len(strings.Split(dealRun(t, "report", "--deal", id)["trail"].(string), "\n")) < 16 {
		dealRun(t, "note", "--deal", id, "--kind", "message", "--input", writeJSON(t, `{"from":"counterparty","text":"noted"}`))
	}

	dir := t.TempDir()
	own, shared := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "shared.html")
	dealRun(t, "report", "--deal", id, "--bundle", own)
	dealRun(t, "report", "--deal", id, "--html", shared, "--share", "counterparty", "--to", "the shop")
	for name, path := range map[string]string{"own copy": own, "shared copy": shared} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var b map[string]any
		if filepath.Ext(path) == ".html" {
			b = embeddedBundle(t, string(raw))
		} else {
			require.NoError(t, json.Unmarshal(raw, &b))
		}
		steps := b["extensions"].(map[string]any)["x-deal-v0"].(map[string]any)["steps"].([]any)
		require.Len(t, steps, 16, name)
		completeness := b["completeness"].(map[string]any)
		assert.Equal(t, "15", fmt.Sprint(completeness["closure_depth"]), "%s: every hop from the latest step back to the opening", name)
		assert.Equal(t, "complete", completeness["records_mode"], name)
		assert.Empty(t, completeness["missing"], name)
		records := map[string]bool{}
		for _, r := range b["records"].([]any) {
			records[r.(map[string]any)["capsule_id"].(string)] = true
		}
		for _, s := range steps {
			assert.True(t, records[s.(map[string]any)["capsule_id"].(string)], "%s: step %v is in the closure", name, s.(map[string]any)["n"])
		}
		assert.Equal(t, steps[15].(map[string]any)["capsule_id"], b["root"], "%s: the root is the latest step", name)
		out, err := invoke(t, "", "verify", "--bundle", path)
		require.NoError(t, err, out)
		var verified map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &verified))
		assert.Equal(t, "pass", verified["graph_closure"].(map[string]any)["status"], name)
	}
}
