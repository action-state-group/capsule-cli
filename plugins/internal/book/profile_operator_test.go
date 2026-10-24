package book

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		require.NoError(t, cli.SaveProfile(p, false))
		loaded, err := cli.LoadProfile(name)
		require.NoError(t, err, "an existing profile without an operator still loads")

		_, err = openBook(t.Context(), loaded, true)
		require.ErrorIs(t, err, cli.ErrInput)
		msg := cli.SafeError(err)
		assert.Contains(t, msg, "profile "+name+" has no operator")
		assert.Contains(t, msg, "capsulectl profile update --profile "+name+" --operator")

		_, err = invoke(t, "", "profile", "update", "--profile", name, "--operator", "Example Operator")
		require.NoError(t, err, "the named fix works")
		repaired, err := cli.LoadProfile(name)
		require.NoError(t, err)
		assert.Equal(t, "Example Operator", repaired.Operator)
	}
}
