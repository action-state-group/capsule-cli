package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dealBuildsDriver loads deal-view.js under node, with a runtime that only
// takes the module's registration, and prints dealBuildsCompared for each
// case given on stdin as {builds, report}.
const dealBuildsDriver = `
const fs = require("fs");
const vm = require("vm");
const context = vm.createContext({ EvidenceGraph: { registerPresentation() {} } });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), context);
const cases = JSON.parse(fs.readFileSync(0, "utf8"));
const compare = vm.runInContext("dealBuildsCompared", context);
process.stdout.write(JSON.stringify(cases.map((c) => compare(c.builds, c.report))));
`

// The deal page compares versions only between builds of the same name
// (capsule-cli#326): on a capsulectl-deal page a step capsulectl sealed, with
// or without a recorded version, is said apart as an earlier build, and the
// link is the plugin's release notes when they are https. A page without
// page_producer reads as it did before.
func TestDealViewComparesBuildsByName(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver.js")
	script := filepath.Join(dir, "deal-view.js")
	require.NoError(t, os.WriteFile(driver, []byte(dealBuildsDriver), 0o600))
	require.NoError(t, os.WriteFile(script, []byte(DealViewJS), 0o600))

	type build struct {
		Name     string  `json:"name"`
		Producer string  `json:"producer"`
		Version  *string `json:"version,omitempty"`
	}
	v := func(s string) *string { return &s }
	core := func(version string) build {
		return build{Name: "capsulectl " + version + " (abc1234)", Producer: "capsulectl", Version: v(version)}
	}
	plugin := func(version string) build {
		return build{Name: "capsulectl-deal " + version + " (def5678)", Producer: "capsulectl-deal", Version: v(version)}
	}
	unrecorded := build{Name: "an earlier capsulectl that did not record its version", Producer: "capsulectl"}
	pluginPage := func(version string) map[string]any {
		return map[string]any{
			"page_version":  "v0.1.0-rc23",
			"page_producer": map[string]any{"name": "capsulectl-deal", "version": version, "commit": "0123456"},
			"release_notes": "https://example.com/deal/releases/tag/" + version,
		}
	}
	type input struct {
		Builds []build        `json:"builds"`
		Report map[string]any `json:"report"`
	}
	cases := []input{
		{[]build{core("v0.1.0-rc21"), unrecorded}, map[string]any{"page_version": "v0.1.0-rc22"}},
		{[]build{core("v0.1.0-rc21")}, pluginPage("v0.1.1")},
		{[]build{unrecorded, core("v0.1.0-rc21"), plugin("v0.1.0"), plugin("v0.1.1")}, pluginPage("v0.1.1")},
		{[]build{plugin("v0.1.1")}, pluginPage("v0.1.1")},
		{[]build{plugin("v0.1.0")}, map[string]any{"page_version": "v0.1.0-rc24", "release_notes": "javascript:alert(1)"}},
	}
	in, err := json.Marshal(cases)
	require.NoError(t, err)
	cmd := exec.Command(node, driver, script)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	type result struct {
		PageBuild     map[string]string `json:"pageBuild"`
		OlderBuilds   []string          `json:"olderBuilds"`
		EarlierBuilds []string          `json:"earlierBuilds"`
		ReleaseNotes  string            `json:"releaseNotes"`
		BuildNote     string            `json:"buildNote"`
	}
	var got []result
	require.NoError(t, json.Unmarshal(out, &got), string(out))
	require.Len(t, got, len(cases))
	const coreNotes = "https://github.com/action-state-group/capsule-cli/releases"

	// A page built inside capsule-cli: the text is the one pages carried
	// before page_producer, and an unrecorded step is an older capsulectl.
	assert.Equal(t, map[string]string{"name": "capsulectl", "version": "v0.1.0-rc22"}, got[0].PageBuild)
	assert.Equal(t, []string{"v0.1.0-rc21", "unrecorded"}, got[0].OlderBuilds)
	assert.Empty(t, got[0].EarlierBuilds)
	assert.Equal(t, coreNotes, got[0].ReleaseNotes)
	assert.Equal(t, "Produced by an older version (v0.1.0-rc21, unrecorded) than the capsulectl that made this page (v0.1.0-rc22).", got[0].BuildNote)

	// A capsulectl step on a capsulectl-deal page is an earlier build, whatever its tag.
	assert.Empty(t, got[1].OlderBuilds)
	assert.Equal(t, []string{"capsulectl v0.1.0-rc21"}, got[1].EarlierBuilds)
	assert.Equal(t, "https://example.com/deal/releases/tag/v0.1.1", got[1].ReleaseNotes)
	assert.Equal(t, "Some steps were sealed by capsulectl v0.1.0-rc21, the build that sealed deals before capsulectl-deal did; capsulectl-deal v0.1.1 made this page.", got[1].BuildNote)

	// An unrecorded step is an earlier capsulectl too, never an older
	// capsulectl-deal; same-name builds compare by tag.
	assert.Equal(t, []string{"v0.1.0"}, got[2].OlderBuilds)
	assert.Equal(t, []string{"an earlier capsulectl that did not record its version", "capsulectl v0.1.0-rc21"}, got[2].EarlierBuilds)
	assert.Equal(t, "Produced by an older version (v0.1.0) than the capsulectl-deal that made this page (v0.1.1). "+
		"Some steps were sealed by an earlier capsulectl that did not record its version, capsulectl v0.1.0-rc21, the builds that sealed deals before capsulectl-deal did; capsulectl-deal v0.1.1 made this page.", got[2].BuildNote)

	// The build that made the page sealed every step: nothing to say.
	assert.Empty(t, got[3].OlderBuilds)
	assert.Empty(t, got[3].EarlierBuilds)
	assert.Empty(t, got[3].BuildNote)

	// Builds of another name are not compared, and a release-notes link that
	// is not https falls back to capsule-cli's releases.
	assert.Empty(t, got[4].OlderBuilds)
	assert.Empty(t, got[4].EarlierBuilds)
	assert.Empty(t, got[4].BuildNote)
	assert.Equal(t, coreNotes, got[4].ReleaseNotes)
}
