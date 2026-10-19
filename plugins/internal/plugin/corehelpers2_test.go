package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/stretchr/testify/require"
)

var (
	fixtureScript  = []byte(`globalThis.exampleView = 1;`)
	fixtureStyle   = []byte(`.example { color: inherit; }`)
	fixtureWording = []byte(`{"wording_pack_version": "aac.wording-pack/v0", "id": "org.example.view.wording/v0", "locale": "en", "entries": {"heading.l0": "What matters"}}`)
)

// moduleSlotFixture is a valid trusted-executable module: a script, a
// stylesheet and a wording pack, each pinned by the manifest.
func moduleSlotFixture() presentationFixture {
	return presentationFixture{
		manifest: map[string]any{
			"spec_version": "aac.presentation-manifest/v0", "id": "org.example.view/v0",
			"presentation_api": "aac.presentation-api/v0", "runtime_min": "0.1.0",
			"trust_class": "trusted-executable", "requires": map[string]any{"bundle_kind": "evidence-bundle/v2"},
			"audiences": []any{"*"}, "formats": []any{"html"}, "fallback": false, "priority": 1,
			"executable": map[string]any{"carrier": "module-slot", "script_sha256": hexSHA256(fixtureScript),
				"style_sha256": []any{hexSHA256(fixtureStyle)}, "wording_sha256": hexSHA256(fixtureWording)},
		},
		files: map[string][]byte{"view.js": fixtureScript, "view.css": fixtureStyle, "wording.json": fixtureWording},
		entries: []map[string]any{
			{"role": "script", "path": "view.js", "sha256": hexSHA256(fixtureScript)},
			{"role": "style", "path": "view.css", "sha256": hexSHA256(fixtureStyle)},
			{"role": "wording", "path": "wording.json", "sha256": hexSHA256(fixtureWording)},
		},
	}
}

// presentationPlugin writes capsulectl-<name> into root: a launcher whose
// handshake offers the presentation and a subcommand, and its files beside
// it.
func presentationPlugin(t *testing.T, root, name string, p presentationFixture) {
	t.Helper()
	for file, body := range p.files {
		path := filepath.Join(root, file)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		for dir := filepath.Dir(path); dir != root; dir = filepath.Dir(dir) {
			require.NoError(t, os.Chmod(dir, 0o755)) // MkdirAll takes the umask
		}
		require.NoError(t, os.WriteFile(path, body, 0o644))
		require.NoError(t, os.Chmod(path, 0o644))
	}
	meta, err := json.Marshal(map[string]any{
		"name": name, "vendor": "Example", "version": "1.0.0", "plugin_api": cli.PluginAPI, "subcommands": []any{"decide"},
		"presentations": []any{map[string]any{"manifest": p.manifest, "files": p.entries}},
	})
	require.NoError(t, err)
	body := "#!/bin/sh\nif [ \"$1\" = cli-plugin-metadata ]; then cat <<'JSON'\n" + string(meta) + "\nJSON\nexit 0; fi\necho ran\n"
	writeLauncher(t, root, "capsulectl-"+name, body, 0o755)
}

// trustPresentationsIn makes root a presentation root for the test, as the
// built-in roots are outside it.
func trustPresentationsIn(t *testing.T, root string) {
	t.Helper()
	old := cli.PresentationRoots
	cli.PresentationRoots = func() []string { return []string{root} }
	t.Cleanup(func() { cli.PresentationRoots = old })
}

// pluginRoot makes an empty trusted plugin root and points
// CAPSULECTL_PLUGIN_ROOTS at it. t.TempDir's directory takes the process
// umask, so under 002 (a stock Ubuntu login) it is group-writable and the trust
// walk rightly refuses it; the root is set to 0755 so these tests pass under
// any umask.
func pluginRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	return root
}

// exampleDealManifest is a specific module that can stand beside the
// built-ins: it forbids the profile tokens the specific built-ins require,
// so no descriptor matches it and one of them (section 4.5).
func exampleDealManifest(scriptSHA256 string) map[string]any {
	return map[string]any{
		"spec_version": "aac.presentation-manifest/v0", "id": "org.example.deal-view/v0",
		"presentation_api": "aac.presentation-api/v0", "runtime_min": "0.1.0",
		"trust_class": "trusted-executable", "requires": map[string]any{"bundle_kind": "evidence-bundle/v2"},
		"forbids":   map[string]any{"profiles": []any{"spec_version:report/v1", "result_version:evidence-result-v0", "spec_version:evaluation-summary/v1"}},
		"audiences": []any{"*"}, "formats": []any{"html"}, "fallback": false, "priority": 1,
		"executable": map[string]any{"carrier": "module-slot", "script_sha256": scriptSHA256,
			"style_sha256": []any{hexSHA256(fixtureStyle)}, "wording_sha256": hexSHA256(fixtureWording)},
	}
}

type buildOutput struct {
	Page           string           `json:"page"`
	Out            string           `json:"out"`
	Format         string           `json:"format"`
	Audience       string           `json:"audience"`
	BundleDigest   string           `json:"bundle_digest"`
	Runtime        map[string]any   `json:"runtime"`
	Modules        []map[string]any `json:"modules"`
	NotIncluded    []map[string]any `json:"not_included"`
	PluginsRefused []map[string]any `json:"plugins_refused"`
	Refusal        map[string]any   `json:"refusal"`
}

type presentationFixture struct {
	manifest map[string]any
	files    map[string][]byte
	entries  []map[string]any
}

func writeLauncher(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	require.NoError(t, os.Chmod(path, mode))
	return path
}
