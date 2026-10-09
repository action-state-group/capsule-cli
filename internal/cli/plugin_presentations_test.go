package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// presentationFixture is a plugin's presentation: its manifest, files (by
// name, written beside the launcher) and the file entries it lists.
type presentationFixture struct {
	manifest map[string]any
	files    map[string][]byte
	entries  []map[string]any
}

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
		require.NoError(t, os.WriteFile(filepath.Join(root, file), body, 0o644))
	}
	meta, err := json.Marshal(map[string]any{
		"name": name, "vendor": "Example", "version": "1.0.0", "plugin_api": pluginAPI, "subcommands": []any{"decide"},
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
	old := presentationRoots
	presentationRoots = func() []string { return []string{root} }
	t.Cleanup(func() { presentationRoots = old })
}

func discovered(t *testing.T, name string) pluginInfo {
	t.Helper()
	for _, p := range discoverPlugins() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("plugin %s not discovered", name)
	return pluginInfo{}
}

// A plugin's presentation is loaded: capsulectl read each file itself and
// it hashes to the digest the manifest pins.
func TestAPluginsPresentationLoads(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	presentationPlugin(t, root, "view", moduleSlotFixture())
	p := discovered(t, "view")
	require.Empty(t, p.refusal)
	require.Len(t, p.presentations, 1)
	m := p.presentations[0]
	assert.Equal(t, "org.example.view/v0", m.ID)
	assert.Equal(t, "trusted-executable", m.TrustClass)
	require.NotNil(t, m.Script)
	assert.Equal(t, fixtureScript, m.Script.Bytes)
	require.Len(t, m.Styles, 1)
	assert.Equal(t, fixtureStyle, m.Styles[0].Bytes)
	require.NotNil(t, m.Wording)

	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	var ls struct {
		Plugins []map[string]any `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &ls))
	require.Len(t, ls.Plugins, 1)
	assert.Equal(t, []any{map[string]any{"id": "org.example.view/v0", "trust_class": "trusted-executable",
		"presentation_api": "aac.presentation-api/v0", "runtime_min": "0.1.0"}}, ls.Plugins[0]["presentations"])
	assert.NotContains(t, ls.Plugins[0], "presentations_refused")
}

// A declarative module is a manifest and a wording pack, no code.
func TestADeclarativePresentationLoads(t *testing.T) {
	root := pluginRoot(t)
	trustPresentationsIn(t, root)
	p := moduleSlotFixture()
	delete(p.manifest, "executable")
	p.manifest["trust_class"] = "declarative"
	p.manifest["declarative"] = map[string]any{"renderer": "aac.declarative-renderer/v0", "wording_sha256": hexSHA256(fixtureWording),
		"fields": []any{map[string]any{"level": "L0", "source": "/body/state", "kind": "identifier", "label_key": "heading.l0"}}}
	p.entries = p.entries[2:]
	presentationPlugin(t, root, "rules", p)
	got := discovered(t, "rules")
	require.Empty(t, got.refusal)
	require.Len(t, got.presentations, 1)
	assert.Nil(t, got.presentations[0].Script)
}

// Every way a presentation can fail refuses all of that plugin's
// presentations, with the reason, and the plugin still dispatches.
func TestAPluginsPresentationIsRefused(t *testing.T) {
	big := make([]byte, presentationFileLimit+1)
	cases := map[string]struct {
		change func(t *testing.T, root string, p *presentationFixture)
		reason string
	}{
		"a file that does not hash to its entry": {func(_ *testing.T, _ string, p *presentationFixture) {
			p.files["view.js"] = []byte("globalThis.other = 1;")
		}, "not the"},
		"an entry the manifest does not pin": {func(_ *testing.T, _ string, p *presentationFixture) {
			other := []byte("globalThis.other = 1;")
			p.files["view.js"] = other
			p.entries[0]["sha256"] = hexSHA256(other)
		}, "not the script_sha256 the manifest pins"},
		"a path out of the plugin's directory": {func(_ *testing.T, _ string, p *presentationFixture) {
			p.entries[0]["path"] = "../view.js"
		}, "not a plain path"},
		"an absolute path": {func(_ *testing.T, root string, p *presentationFixture) {
			p.entries[0]["path"] = filepath.Join(root, "view.js")
		}, "not a plain path"},
		"a symlink out of the plugin's directory": {func(t *testing.T, root string, p *presentationFixture) {
			outside := filepath.Join(t.TempDir(), "elsewhere.js")
			require.NoError(t, os.WriteFile(outside, fixtureScript, 0o644))
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "link.js")))
			p.entries[0]["path"] = "link.js"
		}, "outside the plugin's directory"},
		"a group-writable file": {func(t *testing.T, root string, p *presentationFixture) {
			// made group-writable once written, below
		}, "group-writable"},
		"a file over the size limit": {func(_ *testing.T, _ string, p *presentationFixture) {
			p.files["view.js"] = big
			p.entries[0]["sha256"] = hexSHA256(big)
			p.manifest["executable"].(map[string]any)["script_sha256"] = hexSHA256(big)
		}, "larger than"},
		"an invalid manifest": {func(_ *testing.T, _ string, p *presentationFixture) {
			p.manifest["title"] = "words in a manifest"
		}, "not an aac.presentation-manifest/v0"},
		"a core-runtime module": {func(_ *testing.T, _ string, p *presentationFixture) {
			p.manifest["executable"] = map[string]any{"carrier": "core-runtime"}
			p.entries = nil
		}, "carried in the module slot"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := pluginRoot(t)
			trustPresentationsIn(t, root)
			p := moduleSlotFixture()
			c.change(t, root, &p)
			presentationPlugin(t, root, "view", p)
			if name == "a group-writable file" {
				require.NoError(t, os.Chmod(filepath.Join(root, "view.js"), 0o664))
			}
			got := discovered(t, "view")
			assert.Empty(t, got.presentations)
			assert.Contains(t, got.refusal, c.reason)
			assert.Equal(t, []string{"decide"}, got.Subcommands, "the plugin itself is still discovered")
		})
	}
}

// CAPSULECTL_PLUGIN_ROOTS can name roots for subcommands, but never makes a
// directory a source of presentations: they come only from the built-in roots.
func TestTheRootsOverrideDoesNotTrustPresentations(t *testing.T) {
	root := pluginRoot(t)
	presentationPlugin(t, root, "view", moduleSlotFixture())
	got := discovered(t, "view")
	assert.Equal(t, []string{"decide"}, got.Subcommands, "the override still finds the plugin")
	assert.Empty(t, got.presentations)
	assert.Contains(t, got.refusal, "presentations load only from the built-in plugin roots")
	for _, r := range builtinPluginRoots() {
		assert.False(t, strings.HasPrefix(root, r))
	}
}

// A host that knows no presentations ignores the member: the handshake's
// other members decode as before.
func TestAnOlderHostIgnoresPresentations(t *testing.T) {
	type olderInfo struct {
		Name        string   `json:"name"`
		PluginAPI   string   `json:"plugin_api"`
		Subcommands []string `json:"subcommands,omitempty"`
	}
	p := moduleSlotFixture()
	meta, err := json.Marshal(map[string]any{"name": "view", "plugin_api": pluginAPI, "subcommands": []any{"decide"},
		"presentations": []any{map[string]any{"manifest": p.manifest, "files": p.entries}}})
	require.NoError(t, err)
	var older olderInfo
	require.NoError(t, json.Unmarshal(meta, &older))
	assert.Equal(t, olderInfo{Name: "view", PluginAPI: pluginAPI, Subcommands: []string{"decide"}}, older)
}
