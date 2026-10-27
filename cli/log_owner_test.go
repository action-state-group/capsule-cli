package cli

import (
	"os"
	"path/filepath"
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
		{"jsonl with a log_id and no log reads as a book", Profile{Type: "jsonl", LogID: "l"}, LogBook},
		{"jsonl declared book", Profile{Type: "jsonl", LogID: "l", Log: "book"}, "book"},
		{"jsonl declared cll", Profile{Type: "jsonl", LogID: "l", Log: LogCLL}, ""},
		{"jsonl naming another plugin", Profile{Type: "jsonl", LogID: "l", Log: "ledger"}, "ledger"},
		{"jsonl with no log_id", Profile{Type: "jsonl"}, ""},
		{"sqlite", Profile{Type: "sqlite", LogID: "l"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.p.LogOwner())
			assert.Equal(t, tc.want != "", tc.p.HasPluginLog())
			assert.Equal(t, tc.want == LogBook, tc.p.IsBook())
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
	for _, log := range []string{"ledger", "book", LogCLL} {
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
	p.Type, p.Log = "sqlite", "ledger"
	err := p.validate()
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "only a jsonl profile with a log_id can name a plugin")
	p = base()
	p.LogID, p.Namespace, p.Log = "", "ns", "ledger"
	require.ErrorIs(t, p.validate(), ErrInput)
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

// IsBook means the book plugin, and only it: the book plugin's own checks use
// it, so another plugin's profile is not one.
func TestIsBookNamesTheBookOnly(t *testing.T) {
	ledger := Profile{Type: "jsonl", LogID: "l", Log: "ledger"}
	assert.True(t, ledger.HasPluginLog())
	assert.False(t, ledger.IsBook())
	assert.False(t, ledger.OperatorMissing(), "the operator is the book's")
	book := Profile{Type: "jsonl", LogID: "l", Log: LogBook}
	assert.True(t, book.IsBook())
	assert.True(t, book.OperatorMissing())
	assert.True(t, Profile{Type: "jsonl", LogID: "l"}.IsBook(), "a legacy profile still reads as a book")
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
	p.Type, p.Log, p.Operator = "jsonl", LogBook, "op"
	p.Connection.Database = filepath.Join(t.TempDir(), "store")
	require.NoError(t, SaveProfile(p, false))
	for _, log := range []string{"ledger", LogCLL} {
		_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--log", log)
		require.ErrorIs(t, err, ErrInput, log)
		assert.Contains(t, SafeError(err), "stays with the owner it was made with (log: book)")
	}
	_, err := invoke(t, "", "profile", "update", "--profile", p.Name, "--log", LogBook)
	require.NoError(t, err, "restating the owner is accepted")

	legacy := p
	legacy.Name, legacy.Log = "legacy-book", ""
	require.NoError(t, SaveProfile(legacy, false))
	_, err = invoke(t, "", "profile", "update", "--profile", legacy.Name, "--log", LogBook)
	require.NoError(t, err, "the fix line for a profile saved without an owner")
}

// A plugin that keeps a log in its own files is not retired with --move-data:
// the core holds only the book's locks.
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
