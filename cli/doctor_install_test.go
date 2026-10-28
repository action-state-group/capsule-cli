package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func installCheckOf(t *testing.T, profile, skills string) (map[string]any, error) {
	t.Helper()
	out, err := invoke(t, "", "doctor", "--install-check", "--profile", profile,
		"--expect-version", CLIVersion, "--expect-commit", CLICommit, "--skills-dir", skills)
	var got struct {
		Check map[string]any `json:"install_check"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	return got.Check, err
}

// The install check looks for the skill named like the plugin that keeps the
// profile's log, and refuses a profile that does not say who that is.
func TestInstallCheckFollowsTheProfilesLogOwner(t *testing.T) {
	pluginRoot(t)
	skills := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(skills, "ledger"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skills, "ledger", "SKILL.md"), []byte("---\nname: ledger\n---\n"), 0o644))

	owned := ownedProfile(t, "jsonl", "ledger")
	check, err := installCheckOf(t, owned.Name, skills)
	require.ErrorIs(t, err, ErrPartial, "the plugin is not installed")
	assert.Equal(t, "ledger", check["skill_name"])
	assert.Equal(t, []any{"ledger/SKILL.md"}, check["skills"])
	assert.NotContains(t, check, "deal_skills")

	// A sqlite store only the older marker covers has no declared owner: the
	// check says so instead of passing without looking at any plugin.
	legacy := sqliteStoreProfile(t)
	execSQL(t, legacy, `CREATE TABLE deal_shelf (id INTEGER PRIMARY KEY)`)
	check, err = installCheckOf(t, legacy.Name, skills)
	require.ErrorIs(t, err, ErrPartial)
	assert.Contains(t, check["issues"], "profile "+legacy.Name+" has no declared log owner: capsulectl profile update --profile "+legacy.Name+" --log <cll or the plugin that keeps its log>")
}
