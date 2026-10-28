package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The plugin that keeps a profile's log is declared by the profile, not
// guessed from a plugin name (capsule-cli#428).
func TestLogOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Profile
		want string
	}{
		{"jsonl with a log_id and no log has no declared owner", Profile{Type: "jsonl", LogID: "l"}, ""},
		{"jsonl declared cll", Profile{Type: "jsonl", LogID: "l", Log: LogCLL}, ""},
		{"jsonl naming another plugin", Profile{Type: "jsonl", LogID: "l", Log: "ledger"}, "ledger"},
		{"jsonl with no log_id", Profile{Type: "jsonl"}, ""},
		{"sqlite", Profile{Type: "sqlite", LogID: "l"}, ""},
		{"sqlite namespace with no log", Profile{Type: "sqlite", Namespace: "ns"}, ""},
		{"sqlite namespace naming a plugin", Profile{Type: "sqlite", Namespace: "ns", Log: "ledger"}, "ledger"},
		{"sqlite namespace declared cll", Profile{Type: "sqlite", Namespace: "ns", Log: LogCLL}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.p.LogOwner())
			assert.Equal(t, tc.want != "" && tc.p.Type == "jsonl", tc.p.HasPluginLog())
			assert.Equal(t, tc.want != "" && tc.p.Type == "sqlite", tc.p.HasOwnedStore())
			assert.Equal(t, tc.p.Type == "jsonl" && tc.p.LogID != "" && tc.p.Log == "", tc.p.OwnerUndeclared())
		})
	}
}

func TestLogOwnerIsValidated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := func() Profile {
		p, _ := profileFixture(t)
		p.Type = "jsonl"
		p.Connection.Database = filepath.Join(t.TempDir(), "store")
		return p
	}
	for _, log := range []string{"ledger", "other", LogCLL} {
		p := base()
		p.Log = log
		require.NoError(t, p.validate(), log)
	}
	for _, log := range []string{"bundle", "Bad_Name", "has space", "verify"} {
		p := base()
		p.Log = log
		err := p.validate()
		require.ErrorIs(t, err, ErrInput, log)
		assert.Contains(t, SafeError(err), "log must be cll")
	}
	p := base()
	p.Type, p.Namespace, p.Log = "sqlite", "ns", "ledger"
	require.NoError(t, p.validate(), "a sqlite store with a namespace can be a plugin's")
	assert.True(t, p.HasOwnedStore())
	assert.False(t, p.HasPluginLog(), "the core reads it itself")
	p.Namespace, p.LogID = "", "l"
	err := p.validate()
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "can name a plugin as its log owner")
	p = base()
	p.LogID, p.Namespace, p.Log = "", "ns", "ledger"
	require.ErrorIs(t, p.validate(), ErrInput, "a jsonl profile needs a log_id")
}

// A profile naming a plugin other than book forwards its log commands to that
// plugin, and says which one is missing when it is not installed.
func TestAnyPluginCanKeepALog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := pluginRoot(t)
	p, _ := profileFixture(t)
	p.Type, p.Log = "jsonl", "ledger"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))

	_, err := invoke(t, "", "cll", "list", "--profile", p.Name)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "needs the capsulectl-ledger plugin, which is not installed")

	writeLauncher(t, root, "capsulectl-ledger", firstPartyLauncher("ledger"), 0o755)
	out, err := invoke(t, "", "cll", "list", "--profile", p.Name)
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran capsulectl-ledger: cll list")
}

// A process that serves a plugin's logs runs the command itself.
func TestARegisteredBackendServesItsOwnersProfiles(t *testing.T) {
	p := Profile{Type: "jsonl", LogID: "l", Log: "ledger"}
	assert.Nil(t, backendFor(p))
	RegisterLogBackend("ledger", nil)
	t.Cleanup(func() { delete(logBackends, "ledger") })
	assert.Nil(t, backendFor(p), "a nil backend serves nothing")
	RegisterLogBackend("ledger", fakeBackend{})
	assert.NotNil(t, backendFor(p))
	assert.Nil(t, backendFor(Profile{Type: "jsonl", LogID: "l", Log: "other"}), "another plugin's profile is not served")
	assert.Nil(t, backendFor(Profile{Type: "jsonl", LogID: "l", Log: LogCLL}), "the core's own log has no backend")
	done, err := delegateLog(nil, p, nil)
	require.NoError(t, err)
	assert.False(t, done, "a served profile runs here")
}

type fakeBackend struct{ LogBackend }

// A jsonl profile with a log_id that says nothing about its log is not guessed
// to be anyone's; one that names its owner, or says cll, is.
func TestOwnerUndeclaredIsNotGuessed(t *testing.T) {
	assert.True(t, Profile{Type: "jsonl", LogID: "l"}.OwnerUndeclared())
	assert.False(t, Profile{Type: "jsonl", LogID: "l", Log: LogCLL}.OwnerUndeclared())
	assert.False(t, Profile{Type: "jsonl", LogID: "l", Log: "ledger"}.OwnerUndeclared())
	assert.False(t, Profile{Type: "jsonl"}.OwnerUndeclared())
	assert.False(t, Profile{Type: "sqlite", Namespace: "ns"}.OwnerUndeclared())
	assert.True(t, Profile{Type: "jsonl", LogID: "l", Log: "ledger"}.OperatorMissing())
	assert.False(t, Profile{Type: "jsonl", LogID: "l", Log: LogCLL}.OperatorMissing())
}

// In the owner's own process, a missing backend is an error and never another
// launch of the owner.
func TestOwnerProcessWithoutBackendDoesNotForwardToItself(t *testing.T) {
	previous := runningPlugin
	runningPlugin = "ledger"
	t.Cleanup(func() { runningPlugin = previous })
	pluginRoot(t) // no launcher: a forward would say "not installed"
	done, err := delegateLog(nil, Profile{Name: "p", Type: "jsonl", LogID: "l", Log: "ledger"}, nil)
	assert.True(t, done)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "registers no backend")
	assert.NotContains(t, SafeError(err), "not installed")
}

// A profile keeps its log owner: the saved owner cannot be swapped by update,
// and the fix line for a profile saved without one is accepted.
func TestLogOwnerCannotBeSwappedByUpdate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type, p.Log, p.Operator = "jsonl", "ledger", "op"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))
	for _, log := range []string{"other", LogCLL} {
		_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--log", log)
		require.ErrorIs(t, err, ErrInput, log)
		assert.Contains(t, SafeError(err), "stays with the owner it was made with (log: ledger)")
	}
	_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--log", "ledger")
	require.NoError(t, err, "restating the owner is accepted")

	legacy := p
	legacy.Name, legacy.Log = "legacy", ""
	require.NoError(t, SaveProfile(legacy, false))
	_, err = invoke(t, "", "profile", "show", legacy.Name)
	require.NoError(t, err, "a profile saved without an owner still loads, so it can be fixed")
	_, err = invoke(t, "", "cll", "list", "--profile", legacy.Name)
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "has no declared log owner: capsulectl profile update --profile legacy --log")
	_, err = invoke(t, "", "profile", "update", "--profile", legacy.Name, "--log", "ledger")
	require.NoError(t, err, "the fix line for a profile saved without an owner")
	fixed, err := LoadProfile(legacy.Name)
	require.NoError(t, err)
	assert.Equal(t, "ledger", fixed.Log)
}

// A plugin that keeps a log in its own files is not retired with --move-data:
// the core holds only its own journal's lock.
func TestRetireMoveDataRefusesAnotherPluginsLog(t *testing.T) {
	retireOn(t, "2026-10-07")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, _ := profileFixture(t)
	p.Type, p.Log = "jsonl", "ledger"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, os.MkdirAll(p.Connection.Database, 0o700))
	require.NoError(t, SaveProfile(p, false))
	_, err := invoke(t, "", "profile", "retire", p.Name, "--move-data", filepath.Join(t.TempDir(), "x"))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--move-data is not supported")
	assert.Contains(t, profileNames(t), p.Name)
	assert.DirExists(t, p.Connection.Database)
}

// countersign request builds a bundle from the core's own log; on a profile
// whose log a plugin keeps it says so and names the way out.
func TestCountersignRequestOnAPluginKeptProfileNamesTheWayOut(t *testing.T) {
	pluginRoot(t)
	p := ownedProfile(t, "jsonl", "ledger")
	_, err := invoke(t, "", "countersign", "request", "--profile", p.Name, "--service", "https://countersign.example/v1", "--window", "2026-Q3", "--root", strings.Repeat("ab", 32))
	require.ErrorIs(t, err, ErrPluginLogProfile)
	assert.Contains(t, SafeError(err), "pass --bundle")
}
