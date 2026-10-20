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

func writeLauncher(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestVerifyTrustedPathRejections(t *testing.T) {
	root := pluginRoot(t)

	valid := writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	require.NoError(t, VerifyTrustedPath(valid), "a user-owned 0755 launcher on the trusted root is accepted")

	// group- and other-writable launchers
	assert.ErrorContains(t, VerifyTrustedPath(writeLauncher(t, root, "capsulectl-gw", fakePlugin, 0o775)), "group-writable")
	assert.ErrorContains(t, VerifyTrustedPath(writeLauncher(t, root, "capsulectl-ow", fakePlugin, 0o707)), "other-writable")

	// group/other-writable INTERMEDIATE directory between the root and the launcher
	gwDir := filepath.Join(root, "gwdir")
	require.NoError(t, os.Mkdir(gwDir, 0o755))
	launcherGW := writeLauncher(t, gwDir, "capsulectl-x", fakePlugin, 0o755)
	require.NoError(t, os.Chmod(gwDir, 0o775)) // chmod after write: umask does not mask chmod
	assert.ErrorContains(t, VerifyTrustedPath(launcherGW), "group-writable")
	owDir := filepath.Join(root, "owdir")
	require.NoError(t, os.Mkdir(owDir, 0o755))
	launcherOW := writeLauncher(t, owDir, "capsulectl-y", fakePlugin, 0o755)
	require.NoError(t, os.Chmod(owDir, 0o707))
	assert.ErrorContains(t, VerifyTrustedPath(launcherOW), "other-writable")

	// launcher outside every trusted root
	outside := t.TempDir()
	assert.ErrorContains(t, VerifyTrustedPath(writeLauncher(t, outside, "capsulectl-z", fakePlugin, 0o755)), "outside the trusted plugin roots")

	// a symlink on the trusted root whose real target escapes the roots
	target := writeLauncher(t, outside, "capsulectl-real", fakePlugin, 0o755)
	link := filepath.Join(root, "capsulectl-link")
	require.NoError(t, os.Symlink(target, link))
	assert.ErrorContains(t, VerifyTrustedPath(link), "outside the trusted plugin roots")

	// a non-regular file (e.g. a directory) on the trusted root is not a launcher
	dir := filepath.Join(root, "capsulectl-dir")
	require.NoError(t, os.Mkdir(dir, 0o755))
	assert.ErrorContains(t, VerifyTrustedPath(dir), "not a regular file")
}

func TestPluginCannotShadowCoreOrBuiltins(t *testing.T) {
	root := pluginRoot(t)
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
	// No reserved-name command may be a plugin wrapper: a plugin command's Short is
	// "…(plugin · <vendor>)". This catches an accidentally-registered help/completion
	// or verify plugin even before cobra initializes its own help/completion.
	for _, cmd := range c.Commands() {
		switch cmd.Name() {
		case "verify", "help", "completion":
			assert.NotContains(t, cmd.Short, "(plugin", cmd.Name()+" must be the core/builtin command, never a plugin")
		}
	}
	// the core verify command keeps its own Short, not the plugin wrapper's
	for _, cmd := range c.Commands() {
		if cmd.Name() == "verify" {
			assert.NotContains(t, cmd.Short, "plugin", "verify remains the core command")
		}
	}
}

func TestDiscoverAndHandshake(t *testing.T) {
	root := pluginRoot(t)
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
	root := pluginRoot(t)
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

func TestDiscoveryRequiresTheMetadataNameToMatchTheFilename(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	// capsulectl-alias answers as "guard": dispatch would use the metadata name
	// while discovery deduplicates on the filename, so it is refused.
	alias := writeLauncher(t, root, "capsulectl-alias", fakePlugin, 0o755)

	plugins, refused := discoverPluginsAndRefusals()
	require.Len(t, plugins, 1)
	assert.Equal(t, "guard", plugins[0].Name)
	assert.Equal(t, filepath.Join(root, "capsulectl-guard"), plugins[0].path)
	require.Len(t, refused, 1)
	assert.Equal(t, alias, refused[0].Path)
	assert.Equal(t, `alias: metadata names the command "guard", but the launcher's filename names "alias"; they must match`, refused[0].Reason)

	// `plugin ls` says why the launcher is missing.
	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err, out)
	var ls struct {
		Plugins []map[string]any `json:"plugins"`
		Refused []refusedPlugin  `json:"refused"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &ls), out)
	require.Len(t, ls.Plugins, 1)
	assert.Equal(t, refused, ls.Refused)

	// and it is never wired up as a command
	_, err = invoke(t, "", "alias")
	require.Error(t, err)
}

func TestDiscoveryDedupesOnTheFilename(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(first, 0o755))
	require.NoError(t, os.Chmod(second, 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", first+string(os.PathListSeparator)+second)

	// The same filename on two roots: the earlier root wins.
	writeLauncher(t, first, "capsulectl-guard", fakePlugin, 0o755)
	writeLauncher(t, second, "capsulectl-guard", strings.Replace(fakePlugin, "ACME Compliance", "Second Root", 1), 0o755)
	// A refused launcher does not shadow a valid one of the same filename later.
	writeLauncher(t, first, "capsulectl-tool", fakePlugin, 0o755) // says "guard"
	writeLauncher(t, second, "capsulectl-tool", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"tool"`, 1), 0o755)

	plugins, refused := discoverPluginsAndRefusals()
	require.Len(t, plugins, 2)
	assert.Equal(t, "guard", plugins[0].Name)
	assert.Equal(t, "ACME Compliance", plugins[0].Vendor)
	assert.Equal(t, filepath.Join(first, "capsulectl-guard"), plugins[0].path)
	assert.Equal(t, "tool", plugins[1].Name)
	assert.Equal(t, filepath.Join(second, "capsulectl-tool"), plugins[1].path)
	require.Len(t, refused, 1)
	assert.Equal(t, filepath.Join(first, "capsulectl-tool"), refused[0].Path)
}

// envPlugin prints the two variables core hands a launcher.
const envPlugin = `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"env","vendor":"ACME Compliance","version":"1.0.0","plugin_api":"cli-plugin/v1","subcommands":["show"]}'
  exit 0
fi
echo "api=$CAPSULECTL_PLUGIN_API bin=$CAPSULECTL_BIN"
`

func TestPluginGetsCapsulectlBin(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-env", envPlugin, 0o755)
	self, err := selfExecutable()
	require.NoError(t, err)

	dispatch := func() string {
		c := NewCommand()
		var buf bytes.Buffer
		c.SetOut(&buf)
		c.SetErr(&buf)
		c.SetArgs([]string{"env", "show"})
		require.NoError(t, c.ExecuteContext(t.Context()))
		return buf.String()
	}

	// Unset: core hands over its own path.
	t.Setenv("CAPSULECTL_BIN", "")
	require.NoError(t, os.Unsetenv("CAPSULECTL_BIN"))
	assert.Contains(t, dispatch(), "api=cli-plugin/v1 bin="+self)

	// Already set by the caller: kept as it is.
	t.Setenv("CAPSULECTL_BIN", "/opt/elsewhere/capsulectl")
	assert.Contains(t, dispatch(), "bin=/opt/elsewhere/capsulectl")
}

// argvPlugin answers the handshake as acme and prints $0 when run: for a
// script, the path the kernel executed (argv[0] is the launcher's name; see
// TestLauncherCommandKeepsTheNameAsArgv0).
const argvPlugin = `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"acme","vendor":"t","version":"1","plugin_api":"cli-plugin/v1"}'
  exit 0
fi
echo "VARIANT ran=$0"
`

// twoHopRoot lays out R/capsulectl-acme -> R/a/link -> R/b/acme, a launcher
// whose resolved path (R/b/acme, R/b, R) leaves R/a, which holds the second
// hop, off the walk. It returns the root and the R/a directory.
func twoHopRoot(t *testing.T) (root, a string) {
	t.Helper()
	root = pluginRoot(t)
	a = filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(a, 0o755))
	require.NoError(t, os.Mkdir(b, 0o755))
	writeLauncher(t, b, "acme", strings.Replace(argvPlugin, "VARIANT", "good", 1), 0o755)
	require.NoError(t, os.Symlink(filepath.Join(b, "acme"), filepath.Join(a, "link")))
	require.NoError(t, os.Symlink(filepath.Join(a, "link"), filepath.Join(root, "capsulectl-acme")))
	return root, a
}

func TestTheWalkChecksTheDirectoryHoldingEachHopBelowTheRoot(t *testing.T) {
	root, a := twoHopRoot(t)
	launcher := filepath.Join(root, "capsulectl-acme")
	checked, err := CheckedTrustedPath(launcher)
	require.NoError(t, err, "every hop's directory is safe")
	real, err := filepath.EvalSymlinks(filepath.Join(root, "b", "acme"))
	require.NoError(t, err)
	assert.Equal(t, real, checked, "the checked path is the resolved one")

	require.NoError(t, os.Chmod(a, 0o775))
	_, err = CheckedTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	realA, _ := filepath.EvalSymlinks(a)
	assert.Contains(t, err.Error(), realA+" is group-writable")
	assert.Contains(t, err.Error(), "chmod go-w "+realA, "the refusal names the fix")
	assert.Contains(t, err.Error(), "the launcher "+launcher)
	assert.Empty(t, discoverPlugins(), "discovery refuses it")
}

func TestTheWalkChecksAHopThatLeavesTheRoot(t *testing.T) {
	// R/capsulectl-acme -> W/link -> R/b/acme, with W outside every root:
	// whoever writes W can repoint link.
	root := pluginRoot(t)
	b := filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(b, 0o755))
	writeLauncher(t, b, "acme", strings.Replace(argvPlugin, "VARIANT", "good", 1), 0o755)
	w := filepath.Join(t.TempDir(), "w")
	require.NoError(t, os.Mkdir(w, 0o755))
	require.NoError(t, os.Chmod(filepath.Dir(w), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(b, "acme"), filepath.Join(w, "link")))
	launcher := filepath.Join(root, "capsulectl-acme")
	require.NoError(t, os.Symlink(filepath.Join(w, "link"), launcher))
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(w, 0o777))
	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), "w is group-writable")
}

func TestDispatchRunsThePathItChecked(t *testing.T) {
	// The second hop is swapped between the trust check and the exec, to a
	// file in a directory the walk would refuse. What runs is still the
	// file that was checked, under the name it was found by.
	root, a := twoHopRoot(t)
	evilDir := filepath.Join(root, "evil")
	require.NoError(t, os.Mkdir(evilDir, 0o755))
	writeLauncher(t, evilDir, "acme", strings.Replace(argvPlugin, "VARIANT", "evil", 1), 0o755)
	require.NoError(t, os.Chmod(evilDir, 0o777))
	swapped := false
	pluginBeforeExec = func() {
		if swapped {
			return
		}
		swapped = true
		require.NoError(t, os.Remove(filepath.Join(a, "link")))
		require.NoError(t, os.Symlink(filepath.Join(evilDir, "acme"), filepath.Join(a, "link")))
	}
	t.Cleanup(func() { pluginBeforeExec = func() {} })

	out, err := invoke(t, "", "acme")
	require.NoError(t, err, out)
	assert.True(t, swapped)
	checked, err := filepath.EvalSymlinks(filepath.Join(root, "b", "acme"))
	require.NoError(t, err)
	assert.Contains(t, out, "good ran="+checked, "the checked file ran, by its checked path")
	assert.NotContains(t, out, "evil")

	// The next check refuses the swapped link: its target's directory is
	// writable by others.
	_, err = CheckedTrustedPath(filepath.Join(root, "capsulectl-acme"))
	require.ErrorIs(t, err, errWritable)
	realEvil, _ := filepath.EvalSymlinks(evilDir)
	assert.Contains(t, err.Error(), realEvil+" is")
	_, err = invoke(t, "", "acme")
	require.Error(t, err)
}

func TestDispatchHappyPathsRun(t *testing.T) {
	root := pluginRoot(t)
	// A plain launcher.
	writeLauncher(t, root, "capsulectl-acme", strings.Replace(argvPlugin, "VARIANT", "plain", 1), 0o755)
	out, err := invoke(t, "", "acme")
	require.NoError(t, err, out)
	assert.Contains(t, out, "plain ran=")

	// One safe symlink inside the root, relative.
	require.NoError(t, os.Remove(filepath.Join(root, "capsulectl-acme")))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))
	writeLauncher(t, bin, "acme", strings.Replace(argvPlugin, "VARIANT", "linked", 1), 0o755)
	require.NoError(t, os.Symlink(filepath.Join("bin", "acme"), filepath.Join(root, "capsulectl-acme")))
	out, err = invoke(t, "", "acme")
	require.NoError(t, err, out)
	linked, err := filepath.EvalSymlinks(filepath.Join(bin, "acme"))
	require.NoError(t, err)
	assert.Contains(t, out, "linked ran="+linked)
}

func TestLauncherCommandKeepsTheNameAsArgv0(t *testing.T) {
	cmd := launcherCommand(t.Context(), "/real/b/acme", "/root/capsulectl-acme", "x", "--y")
	assert.Equal(t, "/real/b/acme", cmd.Path, "the checked path is executed")
	assert.Equal(t, []string{"/root/capsulectl-acme", "x", "--y"}, cmd.Args, "argv[0] is the name the launcher was found by")
}

func TestARelativeRootStillDispatches(t *testing.T) {
	// CAPSULECTL_PLUGIN_ROOTS=plugins, relative to the working directory:
	// the path and the root are made absolute before they are compared.
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	t.Chdir(dir)
	require.NoError(t, os.Mkdir("plugins", 0o755))
	writeLauncher(t, "plugins", "capsulectl-acme", strings.Replace(argvPlugin, "VARIANT", "relative", 1), 0o755)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", "plugins")
	checked, err := CheckedTrustedPath(filepath.Join("plugins", "capsulectl-acme"))
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(checked), "the checked path is absolute")
	out, err := invoke(t, "", "acme")
	require.NoError(t, err, out)
	assert.Contains(t, out, "relative ran=")
}

func TestTheHopCheckLeavesTheRootsParentsToTheRootsRule(t *testing.T) {
	// An absolute link inside the root passes through the root's parents
	// (Intel Homebrew's /usr/local is 0775). The walk trusts the system
	// root's parents, so the hop check does too.
	parent := filepath.Join(t.TempDir(), "usr-local")
	root := filepath.Join(parent, "plugins")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755))
	writeLauncher(t, filepath.Join(root, "bin"), "acme", strings.Replace(argvPlugin, "VARIANT", "abs", 1), 0o755)
	require.NoError(t, os.Symlink(filepath.Join(root, "bin", "acme"), filepath.Join(root, "capsulectl-acme")))
	require.NoError(t, os.Chmod(parent, 0o775))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	assert.NoError(t, VerifyTrustedPath(filepath.Join(root, "capsulectl-acme")))
}

func TestALauncherFoundInOneRootResolvingIntoAnother(t *testing.T) {
	// Found in root A, resolving into root B through a hop held in A: A's
	// directories get the strict rule too.
	a := pluginRoot(t)
	b := t.TempDir()
	require.NoError(t, os.Chmod(b, 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", a+string(os.PathListSeparator)+b)
	writeLauncher(t, b, "acme", strings.Replace(argvPlugin, "VARIANT", "b", 1), 0o755)
	hop := filepath.Join(a, "hop")
	require.NoError(t, os.Mkdir(hop, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(b, "acme"), filepath.Join(hop, "link")))
	launcher := filepath.Join(a, "capsulectl-acme")
	require.NoError(t, os.Symlink(filepath.Join(hop, "link"), launcher))
	checked, err := CheckedTrustedPath(launcher)
	require.NoError(t, err)
	realB, _ := filepath.EvalSymlinks(filepath.Join(b, "acme"))
	assert.Equal(t, realB, checked)
	require.NoError(t, os.Chmod(hop, 0o775))
	assert.ErrorIs(t, VerifyTrustedPath(launcher), errWritable)
}
