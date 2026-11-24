package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-viewer/go/pageruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const obligationsV2Key = "eu-ai-act-obligations-report/v2"

// obligationsV2Bundle writes fixture 5's valid bundle reshaped the way the
// eu-ai-act-obligations pack (0.3.0) seals a report: the block under
// eu-ai-act-obligations-report/v2, a finding {id, observed, owner_due} (no
// severity or recommendation) on the one requirement the Result reads not
// met, and contract-report/v1 beside it, which draws the card. The
// extensions are not part of the signed records, so the bundle still verifies.
func obligationsV2Bundle(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/presentation/5-monthly-compliance/bundle.json")
	require.NoError(t, err)
	var bundle map[string]any
	require.NoError(t, json.Unmarshal(raw, &bundle))
	ext := bundle["extensions"].(map[string]any)
	block := ext["eu-ai-act-compliance/v1"].(map[string]any)
	delete(ext, "eu-ai-act-compliance/v1")
	found := 0
	for _, o := range block["obligations"].([]any) {
		for _, r := range o.(map[string]any)["rows"].([]any) {
			row := r.(map[string]any)
			delete(row, "finding")
			if row["criterion_id"] == "art50.disclosure_before_first_turn" {
				row["finding"] = map[string]any{"id": "F-07", "observed": "1 of 4 sessions read not met.", "owner_due": "Example Org · 31 Oct 2026"}
				found++
			}
		}
	}
	require.Equal(t, 1, found)
	ext[obligationsV2Key] = block
	ext["contract-report/v1"] = map[string]any{}
	out, err := json.Marshal(bundle)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bundle.json")
	require.NoError(t, os.WriteFile(path, out, 0o600))
	return path
}

// The page core builds for a v2 obligations report carries the viewer runtime
// that reads the v2 block (capsule-viewer#349), so the finding is drawn as
// what was observed and its owner, not left as an uninterpreted extension.
// Chrome (CAPSULECTL_CHROME) opens the page and reads the drawn finding.
func TestPresentationBuildInterpretsAnObligationsV2FindingBlock(t *testing.T) {
	pluginRoot(t)
	bundle := obligationsV2Bundle(t)
	out := presentationOut(t)
	stdout, err := invoke(t, "", "presentation", "build", "--bundle", bundle, "--out", out)
	require.NoError(t, err, stdout)
	assert.Equal(t, "written", decodeBuild(t, stdout).Page)
	page, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(pageruntime.IIFE), obligationsV2Key), "the runtime core embeds reads the v2 template")
	assert.Contains(t, string(page), `"`+obligationsV2Key+`"`, "the page carries the v2 block")

	binary := os.Getenv("CAPSULECTL_CHROME")
	if binary == "" {
		t.Skip("CAPSULECTL_CHROME is not set")
	}
	opened := startHeadlessChrome(t, binary).open(t, "file://"+out, presentationViews[1])
	var drawn []string
	opened.eval(t, `[...document.querySelectorAll("body *")].filter((e) => e.children.length === 0 && !/^(SCRIPT|STYLE)$/.test(e.tagName)).map((e) => e.textContent.trim()).filter((s) => /^(Finding F-07|1 of 4 sessions read not met\.|Example Org · 31 Oct 2026)$/.test(s))`, &drawn)
	assert.Equal(t, []string{"Finding F-07", "1 of 4 sessions read not met.", "Example Org · 31 Oct 2026"}, drawn)
	var interpreted bool
	opened.eval(t, `[...document.querySelectorAll("tr[data-semantics]")].some((r) => r.textContent.includes("`+obligationsV2Key+`") && r.dataset.semantics === "interpreted")`, &interpreted)
	assert.True(t, interpreted, "the verification page reads the v2 extension row as interpreted")
}
