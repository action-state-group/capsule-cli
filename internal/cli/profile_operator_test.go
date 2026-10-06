package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A jsonl profile with a log_id is an evidence book: every record it seals
// carries the profile's operator, the one party the record names. Such a
// profile is refused without one, at create, at update and at seal time.

func createBook(t *testing.T, name string, extra ...string) error {
	t.Helper()
	args := append([]string{"profile", "create", "--name", name, "--type", "jsonl",
		"--jsonl-path", filepath.Join(t.TempDir(), "store"), "--namespace", "demo", "--log-id", "log-" + name}, extra...)
	_, err := invoke(t, "", args...)
	return err
}

func TestProfileCreateRefusesABookWithoutOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing", nil},
		{"empty", []string{"--operator", ""}},
		{"whitespace", []string{"--operator", "   "}},
		{"zero-width-space", []string{"--operator", "\u200b"}},
		{"byte-order-mark", []string{"--operator", "\ufeff"}},
		{"invisible-mix", []string{"--operator", " \u200b\ufeff\u200d "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := createBook(t, "b-"+tc.name, tc.args...)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), "--operator is required for a jsonl profile with a log_id")
			_, err = loadProfile("b-" + tc.name)
			assert.Error(t, err, "a refused profile is not saved")
		})
	}
	require.NoError(t, createBook(t, "named", "--operator", "Example Operator"))
	p, err := loadProfile("named")
	require.NoError(t, err)
	assert.Equal(t, "Example Operator", p.Operator)
}

func TestProfileUpdateRefusesBlankingABooksOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	require.NoError(t, createBook(t, "book", "--operator", "Example Operator"))
	for _, blank := range []string{"", "  ", "\u200b", "\ufeff"} {
		_, err := invoke(t, "", "profile", "update", "--profile", "book", "--operator", blank)
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "--operator is required for a jsonl profile with a log_id")
	}
	// An update that leaves the operator alone keeps it.
	_, err := invoke(t, "", "profile", "update", "--profile", "book", "--clock-tolerance", "2m")
	require.NoError(t, err)
	p, err := loadProfile("book")
	require.NoError(t, err)
	assert.Equal(t, "Example Operator", p.Operator)
	assert.Equal(t, "2m", p.ClockTolerance)

	// Turning an artifact-only jsonl profile into a book needs an operator too.
	_, err = invoke(t, "", "profile", "create", "--name", "artifacts", "--type", "jsonl",
		"--jsonl-path", filepath.Join(t.TempDir(), "store"), "--namespace", "demo", "--log-id", "")
	require.NoError(t, err)
	_, err = invoke(t, "", "profile", "update", "--profile", "artifacts", "--log-id", "now-a-book")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "--operator is required for a jsonl profile with a log_id")
	_, err = invoke(t, "", "profile", "update", "--profile", "artifacts", "--log-id", "now-a-book", "--operator", "Example Operator")
	require.NoError(t, err)
}

// Profiles that seal no record with the operator still need none: SQLite and
// MySQL profiles (deal and discover records name the profile, not an
// operator), and a jsonl profile without a log_id (artifacts only, no book).
func TestProfileCreateWithoutOperatorWhereNothingSealsIt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"--name", "lite", "--type", "sqlite", "--sqlite-path", filepath.Join(t.TempDir(), "c.db"), "--log-id", "lite-log"},
		{"--name", "my", "--type", "mysql", "--mysql-host", "db.example", "--mysql-database", "capsules", "--log-id", "my-log"},
		{"--name", "art", "--type", "jsonl", "--jsonl-path", filepath.Join(t.TempDir(), "store"), "--namespace", "demo", "--log-id", ""},
	} {
		_, err := invoke(t, "", append([]string{"profile", "create"}, args...)...)
		require.NoError(t, err, strings.Join(args, " "))
	}
}

// A book profile saved without an operator by an earlier capsulectl still
// loads (so `profile update --operator` can repair it), but nothing seals
// with it: opening the book refuses, naming the fix.
func TestABookSavedWithoutOperatorRefusesToSeal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for i, blank := range []string{"", "   ", "\u200b", "\ufeff"} {
		name := fmt.Sprintf("legacy%d", i)
		p, _ := profileFixture(t)
		p.Name, p.Type, p.LogID, p.Operator = name, "jsonl", name+"-log", blank
		p.Connection.Database = filepath.Join(t.TempDir(), "store")
		require.NoError(t, saveProfile(p, false))
		loaded, err := loadProfile(name)
		require.NoError(t, err, "an existing profile without an operator still loads")

		_, err = openBook(t.Context(), loaded, true)
		require.ErrorIs(t, err, ErrInput)
		msg := SafeError(err)
		assert.Contains(t, msg, "profile "+name+" has no operator")
		assert.Contains(t, msg, "capsulectl profile update --profile "+name+" --operator")

		_, err = invoke(t, "", "profile", "update", "--profile", name, "--operator", "Example Operator")
		require.NoError(t, err, "the named fix works")
		repaired, err := loadProfile(name)
		require.NoError(t, err)
		assert.Equal(t, "Example Operator", repaired.Operator)
	}
}

// --interactive asks for the operator once the answers make the profile a book.
func TestProfileCreateInteractiveAsksABookForItsOperator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	args := func(name string) []string {
		return []string{"profile", "create", "--interactive", "--name", name, "--type", "jsonl",
			"--jsonl-path", filepath.Join(t.TempDir(), "store"), "--namespace", "demo"}
	}
	_, err := invoke(t, "asked-log\nExample Operator\n", args("asked")...)
	require.NoError(t, err)
	p, err := loadProfile("asked")
	require.NoError(t, err)
	assert.Equal(t, "asked-log", p.LogID)
	assert.Equal(t, "Example Operator", p.Operator)

	for i, answer := range []string{"", "\u200b", "\ufeff"} {
		_, err = invoke(t, "blank-log\n"+answer+"\n", args(fmt.Sprintf("blank%d", i))...)
		require.ErrorIs(t, err, ErrInput, "answer %q", answer)
		assert.Contains(t, SafeError(err), "--operator is required for a jsonl profile with a log_id")
	}
}

// The operator is stored the same way whether it came from --operator or
// --interactive, for every profile type: without leading or trailing
// whitespace or invisible format characters. What is inside is kept.
func TestProfileOperatorIsStoredTrimmed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for i, tc := range []struct{ given, stored string }{
		{"  alice  ", "alice"},
		{"\ufeffalice\u200b", "alice"},
		{"\t Anna Lee \n", "Anna Lee"},
		{"ali\u200bce", "ali\u200bce"},
	} {
		name := fmt.Sprintf("trim%d", i)
		require.NoError(t, createBook(t, name, "--operator", tc.given))
		p, err := loadProfile(name)
		require.NoError(t, err)
		assert.Equal(t, tc.stored, p.Operator, "--operator %q", tc.given)
	}

	_, err := invoke(t, "asked-log\n  Example Operator \u200b\n", "profile", "create", "--interactive", "--name", "asked",
		"--type", "jsonl", "--jsonl-path", filepath.Join(t.TempDir(), "store"), "--namespace", "demo")
	require.NoError(t, err)
	p, err := loadProfile("asked")
	require.NoError(t, err)
	assert.Equal(t, "Example Operator", p.Operator, "--interactive stores the same trimmed value")

	_, err = invoke(t, "", "profile", "update", "--profile", "asked", "--operator", " bob ")
	require.NoError(t, err)
	p, err = loadProfile("asked")
	require.NoError(t, err)
	assert.Equal(t, "bob", p.Operator, "update stores it trimmed")

	_, err = invoke(t, "", "profile", "create", "--name", "lite", "--type", "sqlite",
		"--sqlite-path", filepath.Join(t.TempDir(), "c.db"), "--log-id", "lite-log", "--operator", " alice ")
	require.NoError(t, err)
	p, err = loadProfile("lite")
	require.NoError(t, err)
	assert.Equal(t, "alice", p.Operator, "a profile that is not a book stores it trimmed too")
}
