package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakePlugin = `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"guard","vendor":"ACME Compliance","version":"1.2.3","plugin_api":"cli-plugin/v1","subcommands":["decide"]}'
  exit 0
fi
echo "guard ran: $*"
`

func writeLauncher(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestVerifyTrustedPathRejections(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)

	valid := writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	require.NoError(t, verifyTrustedPath(valid), "a user-owned 0755 launcher on the trusted root is accepted")

	// group- and other-writable launchers
	assert.ErrorContains(t, verifyTrustedPath(writeLauncher(t, root, "capsulectl-gw", fakePlugin, 0o775)), "group-writable")
	assert.ErrorContains(t, verifyTrustedPath(writeLauncher(t, root, "capsulectl-ow", fakePlugin, 0o707)), "other-writable")

	// group/other-writable INTERMEDIATE directory between the root and the launcher
	gwDir := filepath.Join(root, "gwdir")
	require.NoError(t, os.Mkdir(gwDir, 0o755))
	launcherGW := writeLauncher(t, gwDir, "capsulectl-x", fakePlugin, 0o755)
	require.NoError(t, os.Chmod(gwDir, 0o775)) // chmod after write: umask does not mask chmod
	assert.ErrorContains(t, verifyTrustedPath(launcherGW), "group-writable")
	owDir := filepath.Join(root, "owdir")
	require.NoError(t, os.Mkdir(owDir, 0o755))
	launcherOW := writeLauncher(t, owDir, "capsulectl-y", fakePlugin, 0o755)
	require.NoError(t, os.Chmod(owDir, 0o707))
	assert.ErrorContains(t, verifyTrustedPath(launcherOW), "other-writable")

	// launcher outside every trusted root
	outside := t.TempDir()
	assert.ErrorContains(t, verifyTrustedPath(writeLauncher(t, outside, "capsulectl-z", fakePlugin, 0o755)), "outside the trusted plugin roots")

	// a symlink on the trusted root whose real target escapes the roots
	target := writeLauncher(t, outside, "capsulectl-real", fakePlugin, 0o755)
	link := filepath.Join(root, "capsulectl-link")
	require.NoError(t, os.Symlink(target, link))
	assert.ErrorContains(t, verifyTrustedPath(link), "outside the trusted plugin roots")

	// a non-regular file (e.g. a directory) on the trusted root is not a launcher
	dir := filepath.Join(root, "capsulectl-dir")
	require.NoError(t, os.Mkdir(dir, 0o755))
	assert.ErrorContains(t, verifyTrustedPath(dir), "not a regular file")
}

func TestPluginCannotShadowCoreOrBuiltins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	// launchers named after a core command, both cobra builtins, and a multi-token
	// name (whose cobra name would be the first token, "verify")
	writeLauncher(t, root, "capsulectl-verify", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"verify"`, 1), 0o755)
	writeLauncher(t, root, "capsulectl-help", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"help"`, 1), 0o755)
	writeLauncher(t, root, "capsulectl-completion", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"completion"`, 1), 0o755)
	writeLauncher(t, root, "capsulectl-verify extra", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"verify extra"`, 1), 0o755)

	c := NewCommand()
	byName := map[string]int{}
	for _, cmd := range c.Commands() {
		byName[cmd.Name()]++
	}
	assert.Equal(t, 1, byName["verify"], "the core verify is not shadowed or duplicated by a plugin (incl. the multi-token launcher)")
	assert.LessOrEqual(t, byName["completion"], 1, "a completion plugin does not add a duplicate command")
	assert.LessOrEqual(t, byName["help"], 1, "a help plugin does not add a duplicate command")
	// the core verify command keeps its own Short, not the plugin wrapper's
	for _, cmd := range c.Commands() {
		if cmd.Name() == "verify" {
			assert.NotContains(t, cmd.Short, "plugin", "verify remains the core command")
		}
	}
}

func TestDiscoverAndHandshake(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	// a version-mismatched plugin must be refused, not trusted
	writeLauncher(t, root, "capsulectl-stale", strings.Replace(fakePlugin, "cli-plugin/v1", "cli-plugin/v2", 1), 0o755)
	// a group-writable launcher must be skipped by discovery
	writeLauncher(t, root, "capsulectl-gw", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"gw"`, 1), 0o775)

	plugins := discoverPlugins()
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Name)
	}
	assert.Equal(t, []string{"guard"}, names, "only the trusted, handshake-valid, matching-api launcher is discovered")
	require.Len(t, plugins, 1)
	assert.Equal(t, "ACME Compliance", plugins[0].Vendor)
	assert.Equal(t, []string{"decide"}, plugins[0].Subcommands)

	_, err := pluginMetadata(filepath.Join(root, "capsulectl-stale"), "stale")
	assert.ErrorContains(t, err, "plugin_api")
}

func TestPluginDispatchAndLs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)

	// `plugin ls` lists the discovered plugin with its vendor
	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	var listed struct {
		Plugins []map[string]any `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Plugins, 1)
	assert.Equal(t, "guard", listed.Plugins[0]["name"])
	assert.Equal(t, "ACME Compliance", listed.Plugins[0]["vendor"])

	// `capsulectl guard ...` dispatches to the launcher, passing args through
	c := NewCommand()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"guard", "decide", "--flag"})
	require.NoError(t, c.ExecuteContext(t.Context()))
	assert.Contains(t, buf.String(), "guard ran: decide --flag")
}
