package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptRepo is a throwaway git repository holding a copy of one of this
// repository's scripts under scripts/, so the script's cd to its parent
// lands there, plus files.
func scriptRepo(t *testing.T, script string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile(filepath.Join("..", "scripts", script))
	require.NoError(t, err)
	files["scripts/"+script] = string(body)
	for name, text := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(text), 0o755))
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	return dir
}

// A go list that fails must fail the neutral-modules check, not pass it with
// an empty graph.
func TestCheckNeutralDepsFailsWhenGoListFails(t *testing.T) {
	dir := scriptRepo(t, "check-neutral-deps.sh", map[string]string{})
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	cmd := exec.Command("bash", filepath.Join(dir, "scripts", "check-neutral-deps.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "go list failed")
}

// The moved-paths check reports repository paths into the moved trees, not
// URLs that happen to contain the same words, and --fail turns it into an
// error.
func TestCheckMovedPaths(t *testing.T) {
	dir := scriptRepo(t, "check-moved-paths.sh", map[string]string{
		"docs/a.md": "see skills/deal/SKILL.md\n",
		"docs/b.md": "fetched from https://example.test/api/plugins/cmd/demo\n",
		"docs/c.md": "the agent host's copy: /path/to/the/agent/skills/deal/SKILL.md\n",
		"Makefile":  "\tcd $(root)/plugins/cmd/capsulectl-deal\n",
	})
	run := func(args ...string) (string, error) {
		out, err := exec.Command("bash", append([]string{filepath.Join(dir, "scripts", "check-moved-paths.sh")}, args...)...).CombinedOutput()
		return string(out), err
	}
	out, err := run()
	require.NoError(t, err, "warn mode never fails")
	assert.Contains(t, out, "::warning::docs/a.md references a moved path")
	assert.Contains(t, out, "::warning::Makefile references a moved path")
	assert.NotContains(t, out, "docs/b.md", "a URL is not a repository path")
	assert.NotContains(t, out, "docs/c.md", "an absolute path is not a repository path")

	out, err = run("--fail")
	require.Error(t, err)
	assert.Contains(t, out, "::error::docs/a.md")
}
