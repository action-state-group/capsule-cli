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

// A saved deal profile (no owner declared, namespace deal) goes to the deal
// plugin, with the flags the core no longer needs to know.
func TestBundleRoutesALegacyDealProfileToTheDealPlugin(t *testing.T) {
	firstPartyPlugins(t)
	p, _ := profileFixture(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p.Type, p.Namespace, p.LogID, p.Log = "sqlite", "deal", "", ""
	p.Connection.Database = filepath.Join(t.TempDir(), "deal.db")
	require.NoError(t, SaveProfile(p, false))
	out, err := invoke(t, "", "disclose", "--profile", p.Name, "--deal", "d1", "--share", "counterparty", "--to", "x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran capsulectl-deal: disclose --profile "+p.Name+" --deal d1 --share counterparty --to x")
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
	grafted := &cobra.Command{Use: "cadence", Short: "grafted", RunE: func(c *cobra.Command, _ []string) error { return nil }}
	GraftCommand([]string{"cll", "checkpoint"}, grafted)
	root := NewCommandWith()
	found, _, err := root.Find([]string{"cll", "checkpoint", "cadence"})
	require.NoError(t, err)
	assert.Equal(t, "grafted", found.Short)
	assert.Same(t, grafted, found)

	grafts = nil
	GraftCommand([]string{"cll", "nowhere"}, &cobra.Command{Use: "x"})
	assert.PanicsWithValue(t, `GraftCommand: no core command "nowhere" on the path cll nowhere`, func() { NewCommandWith() })
}
