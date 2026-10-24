package book

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// presentationDir is the core's presentation fixtures; capsulectl-deal's
// TestPresentationGoldens renders and measures every one of them.
const presentationDir = "../../../cli/testdata/presentation"

// TestReportBuildGoldens re-emits each fixture page `book report build`
// wrote and holds the committed page to it byte for byte, so the browser
// checks that read the committed page measure what this plugin writes.
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
			pageFile := filepath.Join(dir, "page.html")
			if os.Getenv("CAPSULECTL_UPDATE_PRESENTATION_PAGES") == "1" {
				require.NoError(t, os.WriteFile(pageFile, page, 0o644))
				return
			}
			// The browser checks (capsulectl-deal's TestPresentationGoldens)
			// measure the committed page, so it must be exactly what this
			// build writes: a page from an older viewer fails here, and
			// CAPSULECTL_UPDATE_PRESENTATION_PAGES=1 rewrites it.
			require.Equal(t, string(page), string(mustRead(t, pageFile)), "the committed page is not what `book report build` writes now; rewrite it with CAPSULECTL_UPDATE_PRESENTATION_PAGES=1")
		})
	}
	require.Positive(t, seen, "no report-build fixture found")
}
