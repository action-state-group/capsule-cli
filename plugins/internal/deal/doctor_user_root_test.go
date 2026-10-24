package deal

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userRootHome is a private HOME whose path passes the trust walk, with a
// v0.1.0-rc3 capsulectl-deal in its per-user root, and a witnessed deal
// profile. It returns HOME resolved and the root.
func userRootHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	tmp := filepath.Clean(os.TempDir())
	for p := home; p != tmp && strings.HasPrefix(p, tmp+string(os.PathSeparator)); p = filepath.Dir(p) {
		info, err := os.Stat(p)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(p, info.Mode().Perm()&^0o022))
	}
	home, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("SUDO_UID", "")
	root := filepath.Join(home, ".local", "lib", "capsulectl", "plugins")
	require.NoError(t, os.MkdirAll(root, 0o755))
	for p := root; p != home; p = filepath.Dir(p) {
		require.NoError(t, os.Chmod(p, 0o755))
	}
	writeDealLauncher(t, root)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cadenceFixture(t, "https://witness.example", pub, "1h", "10m", 1)
	releaseBuild(t, "v0.1.0-rc3", releaseCommit)
	return home, root
}

func writeDealLauncher(t *testing.T, root string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = cli-plugin-metadata ]; then echo '{\"name\":\"deal\",\"vendor\":\"v\",\"version\":\"v0.1.0-rc3\",\"plugin_api\":\"cli-plugin/v1\"}'; fi\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-deal"), []byte(script), 0o755))
}

// unhealable makes the chain one the heal refuses: ~/.local leads to a
// directory outside HOME that others can write.
func unhealable(t *testing.T, home string) {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "shared")
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.Rename(local, filepath.Join(outside, "local")))
	require.NoError(t, os.Chmod(outside, 0o777))
	require.NoError(t, os.Symlink(filepath.Join(outside, "local"), local))
}

func userRootState(check map[string]any) any {
	ur, _ := check["user_root"].(map[string]any)
	return ur["state"]
}

func dealSkills(t *testing.T) string {
	return skillsDir(t, map[string]string{"deal/SKILL.md": dealSkillText})
}

func TestInstallCheckHealsAGroupWritableHome(t *testing.T) {
	home, _ := userRootHome(t)
	require.NoError(t, os.Chmod(home, 0o775))
	check, out, err := installCheckRun(t, dealSkills(t))
	require.NoError(t, err, out)
	assert.Equal(t, true, check["ok"], out)
	assert.Equal(t, "healed", userRootState(check))
	info, err := os.Stat(home)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestInstallCheckFailsWhenTheProfilePluginIsRefused(t *testing.T) {
	home, _ := userRootHome(t)
	unhealable(t, home)
	check, out, err := installCheckRun(t, dealSkills(t))
	require.Error(t, err, out)
	assert.Equal(t, false, check["ok"])
	assert.Equal(t, "refused", userRootState(check))
	assert.Equal(t, []any{"capsulectl-deal is not installed: run capsulectl plugin install deal"}, check["issues"], "the plugin check's issue, and none of user_root's own")
}

func TestInstallCheckPassesWithAValidSystemPluginAndARefusedUserRoot(t *testing.T) {
	home, root := userRootHome(t)
	system := filepath.Join(t.TempDir(), "plugins")
	require.NoError(t, os.MkdirAll(system, 0o755))
	writeDealLauncher(t, system)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", system+string(os.PathListSeparator)+root)
	unhealable(t, home)
	check, out, err := installCheckRun(t, dealSkills(t))
	require.NoError(t, err, out)
	assert.Equal(t, true, check["ok"], out)
	assert.Equal(t, "refused", userRootState(check))
}
