package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDispatchesToActionstatePlugin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-actionstate", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"actionstate"`, 1), 0o755)

	out, err := invoke(t, "", "run", "--dry-run", "some-action")
	require.NoError(t, err)
	assert.Contains(t, out, "guard ran: --dry-run some-action")
}

func TestRunFailsActionablyWithoutActionstatePlugin(t *testing.T) {
	// An empty, otherwise-untouched trusted root: no plugin at all, the
	// "absent" case.
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", t.TempDir())
	_, err := invoke(t, "", "run", "--dry-run")
	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
	msg := SafeError(err)
	assert.Contains(t, msg, "Action State plugin")
	assert.Contains(t, msg, "licence")

	// A discoverable-but-wrong-shaped launcher (fails its own handshake, e.g.
	// an unlicensed build that refuses cli-plugin-metadata) is, from the base
	// binary's point of view, indistinguishable from absent: same message,
	// no separate "unlicensed" code path to test because none exists.
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-actionstate", strings.Replace(fakePlugin, "cli-plugin/v1", "cli-plugin/v2", 1), 0o755)
	_, err = invoke(t, "", "run", "--dry-run")
	require.Error(t, err)
	assert.Contains(t, SafeError(err), "Action State plugin")
}

func TestRunIgnoresAPluginWithADifferentName(t *testing.T) {
	// A discovered plugin that is not named `actionstate` must never satisfy
	// `run`'s dispatch -- it should fail the same actionable way as if no
	// plugin were present at all.
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-guard", fakePlugin, 0o755)
	_, err := invoke(t, "", "run", "--dry-run")
	require.Error(t, err)
	assert.Contains(t, SafeError(err), "Action State plugin")
}

func TestRunNeverShadowedByAPlugin(t *testing.T) {
	// `run` is registered before addPluginCommands, so a launcher literally
	// named `run` must never win the slot the actionstate dispatcher owns.
	root := t.TempDir()
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	writeLauncher(t, root, "capsulectl-run", strings.Replace(fakePlugin, `"name":"guard"`, `"name":"run"`, 1), 0o755)
	c := NewCommand()
	count := 0
	for _, cmd := range c.Commands() {
		if cmd.Name() == "run" {
			count++
			assert.NotContains(t, cmd.Short, "(plugin", "run must stay the core dispatcher, never a plugin wrapper")
		}
	}
	assert.Equal(t, 1, count)
}
