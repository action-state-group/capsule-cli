package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstPartyLauncher is a stand-in for capsulectl-book or capsulectl-deal:
// it answers the handshake and prints what it was run with, the profile it
// inherited included.
func firstPartyLauncher(name string) string {
	return `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"` + name + `","vendor":"action-state-group","version":"test","plugin_api":"cli-plugin/v1"}'
  exit 0
fi
echo "ran capsulectl-` + name + `: $* | profile=$CAPSULECTL_PROFILE"
`
}

// firstPartyPlugins installs both stand-ins on a trusted root.
func firstPartyPlugins(t *testing.T) {
	t.Helper()
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-"+BookPluginName, firstPartyLauncher(BookPluginName), 0o755)
	writeLauncher(t, root, "capsulectl-"+DealPluginName, firstPartyLauncher(DealPluginName), 0o755)
}

// A plain profile needs no plugin: publish, a checkpoint, a bundle and its
// verification all run in the core with no plugin installed.
func TestPlainProfileRunsWithoutPlugins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pluginRoot(t) // an empty root: no plugin anywhere
	p, _ := profileFixture(t)
	p.Type, p.Log = "jsonl", LogCLL
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))
	_, err := invoke(t, "", "store", "init", "--profile", p.Name)
	require.NoError(t, err)
	request := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(request, requestFixture(t), 0o600))
	out, err := invoke(t, "", "publish", "--profile", p.Name, "--request", request)
	require.NoError(t, err, out)
	var published Publication
	require.NoError(t, json.Unmarshal([]byte(out), &published))
	assert.Equal(t, "appended", published.State)
	_, err = invoke(t, "", "cll", "checkpoint", "create", "--profile", p.Name)
	require.NoError(t, err)
	bundle := filepath.Join(t.TempDir(), "bundle.json")
	_, err = invoke(t, "", "bundle", "--profile", p.Name, "--root", published.CapsuleID, "--out", bundle)
	require.NoError(t, err)
	result, err := verifyBundleOutput(t, bundle)
	require.NoError(t, err)
	assert.Equal(t, "VALID", result["verdict"])
	assert.NoDirExists(t, filepath.Join(p.Connection.Database, "book"), "a plain profile keeps no book")
}

// A book profile's log is kept by capsulectl-book: without it, every
// command that would reach the book says so, before it reads or writes
// anything.
func TestBookProfileNeedsTheBookPlugin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pluginRoot(t)
	p, _ := profileFixture(t)
	p.Type = "jsonl"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))
	require.True(t, p.IsBook(), "a jsonl profile with a log_id is a book unless it says cll")
	for _, args := range [][]string{
		{"store", "init"},
		{"publish", "--request", "unread.json"},
		{"cll", "list"},
		{"cll", "append", "--capsule", "unread.json"},
		{"cll", "checkpoint", "create"},
		{"bundle", "--root", strings.Repeat("ab", 32)},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := invoke(t, "", append(args, "--profile", p.Name)...)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), "needs the capsulectl-book plugin, which is not installed")
			assert.Equal(t, 2, ExitCode(err))
		})
	}
	assert.NoDirExists(t, p.Connection.Database, "nothing was written")
}

// The two first-party plugins answer their names even when they are not
// installed, to say what to install.
func TestAMissingFirstPartyPluginSaysWhatToInstall(t *testing.T) {
	pluginRoot(t)
	for _, name := range []string{BookPluginName, DealPluginName} {
		_, err := invoke(t, "", name, "list")
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "needs the capsulectl-"+name+" plugin, which is not installed")
	}
	_, err := invoke(t, "", "close", "--profile", "x")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "capsulectl-book")
}

// Every old entry point of a command that moved into a plugin
// (capsule-cli#238) forwards with its arguments unchanged, to the plugin
// that has it now, and says on stderr what replaced it.
func TestTransitionalAliasesForwardAndSaySo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	firstPartyPlugins(t)
	book, _ := profileFixture(t)
	book.Name, book.Type = "book", "jsonl"
	book.Connection.Database = t.TempDir()
	require.NoError(t, SaveProfile(book, false))
	deal, _ := profileFixture(t)
	deal.Name, deal.Type, deal.Namespace = "deal", "sqlite", "deal"
	deal.Connection.Database = filepath.Join(t.TempDir(), "deal.db")
	require.NoError(t, SaveProfile(deal, false))
	for _, tc := range []struct {
		args        []string
		ran, notice string
	}{
		{[]string{"close", "--profile", "book", "--period", "day"}, "capsulectl-book: close --profile book --period day", "use `capsulectl book close`"},
		{[]string{"reconcile", "--profile", "book"}, "capsulectl-book: reconcile --profile book", "use `capsulectl book reconcile`"},
		{[]string{"request", "--profile", "book", "--request", "r.json"}, "capsulectl-book: request --profile book --request r.json", "use `capsulectl book request"},
		{[]string{"request", "--profile", "deal", "--request", "r.json"}, "capsulectl-deal: request --profile deal --request r.json", "use `capsulectl deal request`"},
		{[]string{"respond", "--profile", "book"}, "capsulectl-book: respond --profile book", "use `capsulectl book respond"},
		{[]string{"respond", "--profile", "deal"}, "capsulectl-deal: respond --profile deal", "use `capsulectl deal respond`"},
		{[]string{"compose", "--deal", "d"}, "capsulectl-deal: compose --deal d", "use `capsulectl deal compose`"},
		{[]string{"result", "build"}, "capsulectl-book: result build", "use `capsulectl book result`"},
		{[]string{"report", "build"}, "capsulectl-book: report build", "use `capsulectl book report`"},
		{[]string{"backfill", "run"}, "capsulectl-book: backfill run", "use `capsulectl book backfill`"},
		{[]string{"store", "migrate", "--profile", "book", "--log-id", "b2"}, "capsulectl-book: migrate --profile book --log-id b2", "use `capsulectl book migrate"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := invoke(t, "", tc.args...)
			require.NoError(t, err, out)
			assert.Contains(t, out, "ran "+tc.ran)
			assert.Contains(t, out, "is deprecated; "+tc.notice)
			assert.Contains(t, out, "the alias is removed in "+AliasRemovedIn)
		})
	}
}

// Each alias's help, and its line in the root help, say it is deprecated,
// name the replacement and the release that removes it.
func TestTransitionalAliasesHelpSaysDeprecated(t *testing.T) {
	firstPartyPlugins(t)
	root, err := invoke(t, "", "--help")
	require.NoError(t, err)
	for _, alias := range []struct{ name, replacement string }{
		{"close", "book close"}, {"reconcile", "book reconcile"}, {"request", "book request"},
		{"respond", "book respond"}, {"compose", "deal compose"}, {"result", "book result"},
		{"report", "book report"}, {"backfill", "book backfill"},
	} {
		want := "Deprecated: use `capsulectl " + alias.replacement
		assert.Contains(t, root, want, "the root help's %s line", alias.name)
		help, err := invoke(t, "", alias.name, "--help")
		require.NoError(t, err)
		assert.Contains(t, help, want, "%s --help", alias.name)
		assert.Contains(t, help, "removed in "+AliasRemovedIn)
	}
	help, err := invoke(t, "", "store", "migrate", "--help")
	require.NoError(t, err)
	assert.Contains(t, help, "Deprecated: use `capsulectl book migrate`")
}

// --profile reaches a plugin wherever it is written: before the plugin's
// name (in its arguments, and as CAPSULECTL_PROFILE for the commands it runs
// back in the core), or after it; when it is given twice the later wins, as
// it does for every flag.
func TestProfileReachesThePlugin(t *testing.T) {
	firstPartyPlugins(t)
	for _, tc := range []struct {
		args          []string
		ran, inherits string
	}{
		{[]string{"--profile", "a", "deal", "list"}, "--profile a list", "a"},
		{[]string{"deal", "list", "--profile", "b"}, "list --profile b", "b"},
		{[]string{"deal", "--profile=c", "list"}, "--profile=c list", "c"},
		{[]string{"--profile", "a", "deal", "list", "--profile", "b"}, "--profile a list --profile b", "b"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := invoke(t, "", tc.args...)
			require.NoError(t, err, out)
			assert.Contains(t, out, "ran capsulectl-deal: "+tc.ran+" | profile="+tc.inherits)
		})
	}
}

// A CAPSULECTL_PROFILE exported in the operator's shell selects nothing: the
// plugin sees only the profile this run selected.
func TestAStrayProfileExportDoesNotReachThePlugin(t *testing.T) {
	firstPartyPlugins(t)
	t.Setenv(ProfileEnv, "prod")
	t.Setenv("CAPSULECTL_PLUGIN_API", "")
	out, err := invoke(t, "", "deal", "list")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran capsulectl-deal: list | profile=\n")
	out, err = invoke(t, "", "deal", "list", "--profile", "b")
	require.NoError(t, err, out)
	assert.Contains(t, out, "| profile=b")
}

// A command run back in the core from a plugin uses the profile the plugin
// was run with (CAPSULECTL_PROFILE), unless it names its own.
func TestSelectedFallsBackToTheInheritedProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"inherited", "named"} {
		p, _ := profileFixture(t)
		p.Name = name
		require.NoError(t, SaveProfile(p, false))
	}
	selected := func(args ...string) string {
		var got string
		c := &cobra.Command{Use: "x", RunE: func(c *cobra.Command, _ []string) error {
			p, err := Selected(c)
			got = p.Name
			return err
		}}
		c.Flags().String("profile", "", "")
		c.SetArgs(args)
		require.NoError(t, c.Execute())
		return got
	}
	t.Setenv(ProfileEnv, "inherited")
	t.Setenv("CAPSULECTL_PLUGIN_API", "")
	_, err := invoke(t, "", "cll", "list")
	require.ErrorIs(t, err, ErrInput, "outside a plugin run the variable selects nothing")
	t.Setenv("CAPSULECTL_PLUGIN_API", PluginAPI)
	assert.Equal(t, "inherited", selected())
	assert.Equal(t, "named", selected("--profile", "named"), "--profile wins over the inherited one")
	t.Setenv(ProfileEnv, "")
	_, err = invoke(t, "", "cll", "list")
	require.ErrorIs(t, err, ErrInput, "neither: the command says it needs --profile")
}

// CommandLine rebuilds what a command parsed, so a delegated command runs
// in the plugin exactly as it was asked for here.
func TestCommandLineRebuildsTheInvocation(t *testing.T) {
	var got []string
	root := &cobra.Command{Use: "capsulectl"}
	root.PersistentFlags().String("profile", "", "")
	cll := &cobra.Command{Use: "cll"}
	list := &cobra.Command{Use: "list", RunE: func(c *cobra.Command, args []string) error {
		got = CommandLine(c, args)
		return nil
	}}
	list.Flags().Int("limit", 100, "")
	list.Flags().StringSlice("suppress", nil, "")
	list.Flags().Bool("all", false, "")
	cll.AddCommand(list)
	root.AddCommand(cll)
	root.SetArgs([]string{"--profile", "a", "cll", "list", "--limit", "5", "--suppress", "x", "--suppress", "y,z", "--all", "pos"})
	require.NoError(t, root.Execute())
	assert.Equal(t, []string{"cll", "list", "--all=true", "--limit=5", "--profile=a", "--suppress=x", "--suppress=y", "--suppress=z", "--", "pos"}, got)
}

// A plugin's exit code is capsulectl's, with nothing added to what the
// plugin said; it reads as the error class of its code.
func TestAPluginsExitIsCapsulectls(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-"+DealPluginName, `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"deal","vendor":"v","version":"1","plugin_api":"cli-plugin/v1"}'
  exit 0
fi
echo "capsulectl-deal: invalid input" >&2
exit 2
`, 0o755)
	_, err := invoke(t, "", "deal", "list")
	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
	assert.ErrorIs(t, err, ErrInput)
	assert.Empty(t, SafeError(err), "the plugin already said why")
}

func TestAPluginKilledBySignalSaysSo(t *testing.T) {
	root := pluginRoot(t)
	writeLauncher(t, root, "capsulectl-"+DealPluginName, `#!/bin/sh
if [ "$1" = "cli-plugin-metadata" ]; then
  echo '{"name":"deal","vendor":"v","version":"1","plugin_api":"cli-plugin/v1"}'
  exit 0
fi
kill -9 $$
`, 0o755)
	_, err := invoke(t, "", "deal", "list")
	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, SafeError(err), "plugin stopped")
	assert.Contains(t, SafeError(err), "signal: killed")
}

func TestAPluginThatCannotStartIsNamed(t *testing.T) {
	root := pluginRoot(t)
	path := writeLauncher(t, root, "capsulectl-"+DealPluginName, "#!/nonexistent/interpreter\n", 0o755)
	c := &cobra.Command{}
	c.SetContext(t.Context())
	err := execPlugin(c, pluginInfo{Name: DealPluginName, path: path}, []string{"list"})
	require.Error(t, err)
	assert.Equal(t, 1, ExitCode(err))
	assert.Contains(t, SafeError(err), "plugin deal could not be started")
}
