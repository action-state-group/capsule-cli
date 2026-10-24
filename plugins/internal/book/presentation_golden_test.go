package book

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/require"
)

// presentationDir is the core's presentation fixtures; capsulectl-deal's
// TestPresentationGoldens renders and measures every one of them.
const presentationDir = "../../../cli/testdata/presentation"

// TestReportBuildGoldens re-emits each fixture page `book report build`
// wrote and holds it to the committed page while the vendored viewer is the
// one it was emitted with, so the browser checks that read the committed page
// measure what this plugin writes.
func TestReportBuildGoldens(t *testing.T) {
	entries, err := os.ReadDir(presentationDir)
	require.NoError(t, err)
	seen := 0
	for _, entry := range entries {
		dir := filepath.Join(presentationDir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if !entry.IsDir() || err != nil {
			continue
		}
		var meta struct {
			Rerender string `json:"rerender"`
			Card     string `json:"card"`
		}
		require.NoError(t, json.Unmarshal(raw, &meta))
		if meta.Rerender != "report-build" {
			continue
		}
		seen++
		t.Run(entry.Name(), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			out := filepath.Join(t.TempDir(), "page.html")
			_, err := invoke(t, "", "report", "build", "--bundle", filepath.Join(dir, "bundle.json"), "--card", meta.Card, "--out", out)
			require.NoError(t, err, "the page gate must let this fixture's page through")
			page := mustRead(t, out)
			committed := mustRead(t, filepath.Join(dir, "page.html"))
			if strings.Contains(string(committed), string(cli.EvidenceGraphIIFE)) {
				require.Equal(t, string(committed), string(page), "the re-emitted page differs from the one capsulectl emitted")
			}
		})
	}
	require.Positive(t, seen, "no report-build fixture found")
}
