package cli

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ownedProfile(t *testing.T, typ, owner string) Profile {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type, p.Log = typ, owner
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))
	return p
}

// A command a plugin extends hands its raw line to the plugin that keeps the
// profile's log, so the plugin's own flags reach it.
func TestBundleRoutesItsRawLineToTheLogOwner(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-ledger", firstPartyLauncher("ledger"), 0o755)
	for name, typ := range map[string]string{"jsonl": "jsonl", "sqlite": "sqlite"} {
		t.Run(name, func(t *testing.T) {
			p := ownedProfile(t, typ, "ledger")
			out, err := invoke(t, "", "bundle", "--root", "abc", "--zzz=1", "--profile", p.Name)
			require.NoError(t, err, out)
			assert.Contains(t, out, "ran capsulectl-ledger: bundle --root abc --zzz=1 --profile "+p.Name)
		})
	}
}

func TestBundleKeepsCoreOwnedProfilesAndHelpInTheCore(t *testing.T) {
	pluginRoot(t)
	p := ownedProfile(t, "jsonl", LogCLL)
	_, err := invoke(t, "", "bundle", "--profile", p.Name, "--root", "abc", "--zzz=1")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "has no flag --zzz")

	out, err := invoke(t, "", "bundle", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--root")
	assert.Contains(t, out, "--profile", "the inherited flag is listed")

	_, err = invoke(t, "", "bundle", "stray", "--profile", p.Name)
	require.ErrorIs(t, err, ErrInput, "positional arguments are still refused")
	assert.Contains(t, SafeError(err), "takes no positional arguments")

	_, err = invoke(t, "", "bundle", "--root", "abc")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "needs --profile")
}

// The owner's own process serves the command: it never launches itself.
func TestBundleInTheOwnersProcessParsesItsFlags(t *testing.T) {
	pluginRoot(t) // nothing installed: a forward would say so
	p := ownedProfile(t, "sqlite", "ledger")
	previous := runningPlugin
	runningPlugin = "ledger"
	t.Cleanup(func() { runningPlugin = previous })
	_, err := invoke(t, "", "bundle", "--profile", p.Name, "--zzz=1")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "has no flag --zzz")
	assert.NotContains(t, SafeError(err), "not installed")
}

func TestBundleExtensionAddsFlagsAndMayHandleTheRun(t *testing.T) {
	previous := bundleExtensions
	t.Cleanup(func() { bundleExtensions = previous })
	var seen []string
	ExtendBundle(BundleExtension{
		Flags: func(f *pflag.FlagSet, use string) { f.String("zzz", "", "A plugin's flag for "+use) },
		Run: func(c *cobra.Command, use string) (bool, error) {
			zzz, _ := c.Flags().GetString("zzz")
			if zzz == "" {
				return false, nil
			}
			seen = append(seen, use+"="+zzz)
			return true, nil
		},
	})
	pluginRoot(t)
	p := ownedProfile(t, "jsonl", LogCLL)
	for _, use := range []string{"bundle", "disclose", "permalink"} {
		_, err := invoke(t, "", use, "--profile", p.Name, "--zzz", "v")
		require.NoError(t, err, use)
	}
	assert.Equal(t, []string{"bundle=v", "disclose=v", "permalink=v"}, seen)
	out, err := invoke(t, "", "bundle", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "A plugin's flag for bundle")
}

func TestGraftCommandReplacesAChildAndRefusesAnUnknownPath(t *testing.T) {
	previous := grafts
	t.Cleanup(func() { grafts = previous })
	built := 0
	GraftCommand([]string{"cll", "checkpoint"}, func() *cobra.Command {
		built++
		return &cobra.Command{Use: "cadence", Short: "grafted", RunE: func(c *cobra.Command, _ []string) error { return nil }}
	})
	root := NewCommandWith()
	found, _, err := root.Find([]string{"cll", "checkpoint", "cadence"})
	require.NoError(t, err)
	assert.Equal(t, "grafted", found.Short)
	// Each tree gets a command of its own: one that kept the flags of a run
	// (a --help, say) would print help the next time.
	other, _, err := NewCommandWith().Find([]string{"cll", "checkpoint", "cadence"})
	require.NoError(t, err)
	assert.NotSame(t, found, other)
	assert.Equal(t, 2, built)

	grafts = nil
	GraftCommand([]string{"cll", "nowhere"}, func() *cobra.Command { return &cobra.Command{Use: "x"} })
	assert.PanicsWithValue(t, `GraftCommand: no core command "nowhere" on the path cll nowhere`, func() { NewCommandWith() })
}

// --help in any form shows help and runs nothing: a disclose that parsed
// --help=true and carried on would put a disclosure on record.
func TestBundleHelpInAnyFormRunsNothing(t *testing.T) {
	pluginRoot(t)
	p := ownedProfile(t, "jsonl", LogCLL)
	for _, help := range []string{"--help", "-h", "--help=true"} {
		out, err := invoke(t, "", "disclose", "--profile", p.Name, "--root", "abc", help)
		require.NoError(t, err, help)
		assert.Contains(t, out, "--root", help)
	}
	assert.NoDirExists(t, p.Connection.Database, "nothing was opened or written")
}

// The profile a line selects is read with the command's own flags: a flag's
// value that looks like --profile=x does not select x.
func TestRoutingReadsTheProfileWithTheCommandsOwnFlags(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-ledger", firstPartyLauncher("ledger"), 0o755)
	core := ownedProfile(t, "jsonl", LogCLL)
	owned := core
	owned.Name, owned.Log = "ledger-profile", "ledger"
	owned.Connection.Database = filepath.Join(t.TempDir(), "other")
	require.NoError(t, SaveProfile(owned, false))

	// --out takes the next token as its file name, so this selects the core profile.
	out, err := invoke(t, "", "bundle", "--profile", core.Name, "--root", "abc", "--out", "--profile="+owned.Name)
	assert.NotContains(t, out, "ran capsulectl-ledger", "the core profile stays selected (err: %v)", err)
	assert.NotContains(t, SafeError(err), "capsulectl-ledger")
	// The same words with the plugin's profile selected last do route.
	out, err = invoke(t, "", "bundle", "--root", "abc", "--zzz", "--profile", owned.Name)
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran capsulectl-ledger: bundle")
}

// Until the plugin owns writing through a handle of its own, the core writes
// no store a plugin owns.
func TestTheCoreDoesNotWriteAStoreAPluginOwns(t *testing.T) {
	pluginRoot(t)
	p := ownedProfile(t, "sqlite", "ledger")
	p.Namespace = "ns"
	require.NoError(t, SaveProfile(p, true))
	for _, args := range [][]string{{"store", "init"}, {"cll", "checkpoint", "create"}} {
		_, err := invoke(t, "", append(args, "--profile", p.Name)...)
		require.ErrorIs(t, err, ErrPluginStoreWrite, args)
	}
}

// The plugin inherits the profile the routing selected, not one a rescan of
// the raw line would find.
func TestRoutedPluginInheritsTheSelectedProfile(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-ledger", firstPartyLauncher("ledger"), 0o755)
	owned := ownedProfile(t, "jsonl", "ledger")
	out, err := invoke(t, "", "bundle", "--profile", owned.Name, "--root", "abc", "--out", "--profile=other")
	require.NoError(t, err, out)
	assert.Contains(t, out, "| profile="+owned.Name)

	t.Setenv("CAPSULECTL_PLUGIN_API", PluginAPI)
	t.Setenv(ProfileEnv, owned.Name)
	out, err = invoke(t, "", "bundle", "--root", "abc")
	require.NoError(t, err, out)
	assert.Contains(t, out, "| profile="+owned.Name)
}

func TestRoutingLineForms(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-ledger", firstPartyLauncher("ledger"), 0o755)
	owned := ownedProfile(t, "jsonl", "ledger")

	// --profile=x, and a profile named before the subcommand, both route.
	out, err := invoke(t, "", "bundle", "--profile="+owned.Name, "--root", "abc", "--zzz")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran capsulectl-ledger: bundle --profile="+owned.Name)
	out, err = invoke(t, "", "--profile", owned.Name, "bundle", "--root", "abc")
	require.NoError(t, err, out)
	assert.Contains(t, out, "| profile="+owned.Name)

	// After "--" nothing is a flag, so no profile is selected there.
	_, err = invoke(t, "", "bundle", "--root", "abc", "--", "--profile", owned.Name)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "takes no positional arguments")

	// A profile that does not exist is reported, not the plugin's flags.
	_, err = invoke(t, "", "bundle", "--profile", "nope", "--zzz")
	require.ErrorIs(t, err, ErrInput)
	assert.NotContains(t, SafeError(err), "has no flag")
	assert.Contains(t, SafeError(err), "nope")
}

func TestRoutingToAPluginThatIsNotInstalledSaysSo(t *testing.T) {
	pluginRoot(t)
	owned := ownedProfile(t, "sqlite", "ledger")
	_, err := invoke(t, "", "bundle", "--profile", owned.Name, "--root", "abc")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "needs the capsulectl-ledger plugin, which is not installed")
}

func TestGraftNeedsAPath(t *testing.T) {
	assert.PanicsWithValue(t, "GraftCommand: a path names the core command to graft under", func() {
		GraftCommand(nil, func() *cobra.Command { return &cobra.Command{Use: "x"} })
	})
}
