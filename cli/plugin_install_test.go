package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// safeTempDir is a t.TempDir whose path passes install's full-path check
// under any umask: t.TempDir's directories take the umask, and under 002
// they are group-writable, which install rightly refuses. os.TempDir itself
// (/tmp on Linux: root's, sticky) is left alone; the check accepts it.
func safeTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tmp := filepath.Clean(os.TempDir())
	for p := dir; p != tmp && strings.HasPrefix(p, tmp+string(os.PathSeparator)); p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil && info.Mode()&0o022 != 0 {
			require.NoError(t, os.Chmod(p, info.Mode().Perm()&^0o022))
		}
	}
	return dir
}

// installHome makes a private HOME whose path passes install's full-path
// check under any umask, and points the plugin roots at its per-user root
// (which does not exist yet). It returns HOME and that root.
func installHome(t *testing.T) (string, string) {
	t.Helper()
	home := safeTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("SUDO_UID", "")
	root := userPluginRoot(home)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	return home, root
}

// launcherScript answers the handshake with name and version, and records
// every run in marker when it is set.
func launcherScript(name, version, marker string) string {
	record := ""
	if marker != "" {
		record = "echo ran >> " + marker + "\n"
	}
	return fmt.Sprintf("#!/bin/sh\n%sif [ \"$1\" = cli-plugin-metadata ]; then echo '{\"name\":%q,\"vendor\":\"t\",\"version\":%q,\"plugin_api\":\"cli-plugin/v1\"}'; exit 0; fi\necho \"ran %s: $*\"\n", record, name, version, name)
}

func launcherFile(t *testing.T, name, version, marker string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "capsulectl-"+name)
	require.NoError(t, os.WriteFile(path, []byte(launcherScript(name, version, marker)), 0o755))
	return path
}

func installOut(t *testing.T, out string) pluginInstallResult {
	t.Helper()
	var r pluginInstallResult
	require.NoError(t, json.Unmarshal([]byte(out), &r), out)
	return r
}

func noLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), pluginInstallPrefix), "a temporary file was left: %s", e.Name())
	}
}

func TestPluginInstallFromAFile(t *testing.T) {
	_, root := installHome(t)
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1.2", ""))
	require.NoError(t, err, out)
	r := installOut(t, out)
	assert.Equal(t, "acme", r.Name)
	assert.Equal(t, "1.2", r.Version)
	canonical, _, _, err := canonicalPath(root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonical, "capsulectl-acme"), r.Path)
	assert.Empty(t, r.ReplacedVersion)
	info, err := os.Stat(r.Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	for p := root; p != filepath.Dir(filepath.Join(os.Getenv("HOME"), ".local")); p = filepath.Dir(p) {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o022, "%s was created writable by others", p)
	}
	noLeftovers(t, root)

	// Discovery and dispatch see it.
	out, err = invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, `"name":"acme"`)
	out, err = invoke(t, "", "acme", "hello")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran acme: hello")

	// Installing again replaces it and says what it replaced.
	out, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1.3", ""))
	require.NoError(t, err, out)
	assert.Equal(t, "1.2", installOut(t, out).ReplacedVersion)
}

func TestPluginInstallRefusesABadLauncher(t *testing.T) {
	_, root := installHome(t)
	bad := filepath.Join(t.TempDir(), "broken")
	require.NoError(t, os.WriteFile(bad, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	for name, path := range map[string]string{
		"no handshake":  bad,
		"core command":  launcherFile(t, "verify", "1", ""),
		"bad name":      launcherFile(t, "Acme", "1", ""),
		"dotted name":   launcherFile(t, "a.b", "1", ""),
		"empty version": launcherFile(t, "ok-but", "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(t, "", "plugin", "install", path)
			if name == "empty version" {
				require.NoError(t, err, "a third-party plugin needs no version")
				return
			}
			require.ErrorIs(t, err, ErrInput)
			noLeftovers(t, root)
		})
	}
	_, err := os.Stat(filepath.Join(root, "capsulectl-verify"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPluginInstallOperands(t *testing.T) {
	installHome(t)
	_, err := invoke(t, "", "plugin", "install", "capsulectl-acme")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "give it as ./capsulectl-acme")

	// A file named deal in the working directory never turns `deal` into a
	// file install: the name form is refused with the file form.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deal"), []byte(launcherScript("deal", "x", "")), 0o755))
	t.Chdir(dir)
	_, err = invoke(t, "", "plugin", "install", "deal")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "capsulectl plugin install ./capsulectl-deal")
	out, err := invoke(t, "", "plugin", "install", "./deal")
	require.NoError(t, err, out)
	assert.Equal(t, "deal", installOut(t, out).Name)
}

func TestPluginInstallFirstPartyVersion(t *testing.T) {
	installHome(t)
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })

	CLIVersion = "0.1.0-dev"
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "deal", "anything", ""))
	require.NoError(t, err, out)
	assert.Contains(t, installOut(t, out).VersionCheck, "not checked")

	CLIVersion = "v0.2.0"
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "book", "v0.1.9", ""))
	require.ErrorIs(t, err, ErrConflict)
	out, err = invoke(t, "", "plugin", "install", "--allow-version-mismatch", launcherFile(t, "book", "v0.1.9", ""))
	require.NoError(t, err, out)
	assert.Contains(t, installOut(t, out).VersionCheck, "mismatch allowed")
	out, err = invoke(t, "", "plugin", "install", launcherFile(t, "book", "v0.2.0", ""))
	require.NoError(t, err, out)
	r := installOut(t, out)
	assert.Equal(t, "matches capsulectl v0.2.0", r.VersionCheck)
	assert.Equal(t, "v0.1.9", r.ReplacedVersion)
}

func TestPluginInstallRefusesSudoBeforeRunningAnything(t *testing.T) {
	installHome(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("SUDO_UID", "501")
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", marker))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "never through sudo")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the candidate never ran")

	// HOME owned by someone else (a slim container's HOME=/) names HOME.
	t.Setenv("SUDO_UID", "")
	if os.Geteuid() != 0 {
		t.Setenv("HOME", "/")
		_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", marker))
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "HOME=/ is owned by uid 0")
	}
}

func TestPluginManagementSkipsDiscovery(t *testing.T) {
	root := newCommand(false, nil)
	assert.True(t, isPluginManagement(root, []string{"plugin", "install", "deal"}))
	assert.True(t, isPluginManagement(root, []string{"--profile", "p", "plugin", "remove", "deal"}))
	assert.False(t, isPluginManagement(root, []string{"plugin", "ls"}))
	assert.False(t, isPluginManagement(root, []string{"deal", "list"}))
}

func TestPluginInstallRefusesAnUnsafePath(t *testing.T) {
	// A sticky ~/.local is shared on purpose: install neither heals nor
	// uses it (capsule-cli#354 heals only what #339 heals).
	home, root := installHome(t)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.MkdirAll(filepath.Join(local, "lib"), 0o755))
	require.NoError(t, unix.Chmod(local, 0o1775))
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", marker))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "group-writable")
	assert.Contains(t, SafeError(err), "chmod go-w")
	assert.Equal(t, uint32(0o1775), modeOf(t, local), "install changed nothing")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing ran on an unsafe path")
	_, statErr = os.Stat(root)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing was created")

	// Dispatch refuses a plugin placed by hand under the same ~/.local
	// (capsule-cli#270).
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-acme"), []byte(launcherScript("acme", "1", marker)), 0o755))
	_, err = invoke(t, "", "acme", "x")
	require.Error(t, err)
	_, statErr = os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "dispatch ran nothing on an unsafe path")

	// After the printed fix, dispatch runs it and install succeeds.
	require.NoError(t, os.Chmod(local, 0o755))
	out, err := invoke(t, "", "acme", "x")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran acme: x")
	out, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.NoError(t, err, out)
}

func TestPluginInstallHealsAGroupWritableHome(t *testing.T) {
	// A host made HOME and ~/.local group-writable before the first install
	// (capsule-cli#354): install removes that write access, says so, and
	// installs.
	home, root := installHome(t)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Mkdir(local, 0o755))
	chmodOwnGroup(t, home, 0o2770)
	require.NoError(t, os.Chmod(local, 0o775))
	real, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	realLocal := filepath.Join(real, ".local")

	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err, out)
	r := installOut(t, out)
	assert.Equal(t, "acme", r.Name)
	require.NotNil(t, r.RootHealed, out)
	assert.Equal(t, "healed", r.RootHealed.State)
	assert.Equal(t, []string{real, realLocal}, r.RootHealed.ChmodGoW)
	assert.Equal(t, map[string][2]string{real: {"2770", "2750"}, realLocal: {"775", "755"}}, r.RootHealed.Modes)
	assert.Equal(t, installPathHealedReason, r.RootHealed.Reason)
	assert.Equal(t, uint32(0o2750), modeOf(t, home))
	assert.Equal(t, uint32(0o755), modeOf(t, local))
	assert.FileExists(t, filepath.Join(root, "capsulectl-acme"))

	// The installed root loosened later is healed by the next install.
	require.NoError(t, os.Chmod(root, 0o775))
	out, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.NoError(t, err, out)
	r = installOut(t, out)
	require.NotNil(t, r.RootHealed, out)
	assert.Equal(t, []string{filepath.Join(real, ".local", "lib", "capsulectl", "plugins")}, r.RootHealed.ChmodGoW)

	// With nothing to heal, install reports no heal.
	out, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "3", ""))
	require.NoError(t, err, out)
	assert.NotContains(t, out, "plugin_root_healed")
}

func TestPluginInstallHealsADirectoryALinkIsLookedUpIn(t *testing.T) {
	// ~/.local/lib links to ~/.libs, so the canonical path skips ~/.local;
	// the walk still looks lib up in it, and the first install heals it.
	home, root := installHome(t)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Mkdir(local, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".libs"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(home, ".libs"), filepath.Join(local, "lib")))
	require.NoError(t, os.Chmod(local, 0o775))
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err, out)
	r := installOut(t, out)
	require.NotNil(t, r.RootHealed, out)
	assert.Equal(t, []string{filepath.Join(resolved(t, home), ".local")}, r.RootHealed.ChmodGoW)
	assert.FileExists(t, filepath.Join(root, "capsulectl-acme"))
}

func TestPluginInstallSaysWhatItHealedWhenItFails(t *testing.T) {
	t.Run("a chmod fails", func(t *testing.T) {
		home, root := installHome(t)
		local := filepath.Join(home, ".local")
		require.NoError(t, os.Mkdir(local, 0o775))
		require.NoError(t, os.Chmod(local, 0o775))
		require.NoError(t, os.Chmod(home, 0o775))
		calls := 0
		seam(t, &healChmod, func(fd int, mode uint32) error {
			if calls++; calls == 2 {
				return errors.New("injected")
			}
			return unix.Fchmod(fd, mode)
		})
		_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "install had already removed group and other write from "+strconv.Quote(resolved(t, home)))
		assert.Contains(t, SafeError(err), "chmod go-w on "+filepath.Join(resolved(t, home), ".local")+": injected")
		assert.Equal(t, uint32(0o755), modeOf(t, home))
		assert.NoDirExists(t, root)
	})
	t.Run("every chmod fails", func(t *testing.T) {
		home, root := installHome(t)
		require.NoError(t, os.Chmod(home, 0o775))
		seam(t, &healChmod, func(int, uint32) error { return errors.New("injected") })
		_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "chmod go-w on "+resolved(t, home)+": injected")
		assert.NotContains(t, SafeError(err), "already removed")
		assert.Equal(t, uint32(0o775), modeOf(t, home))
		assert.NoDirExists(t, root)
	})
	t.Run("a hidden message", func(t *testing.T) {
		// An error SafeError shows only by class keeps its message; the
		// note goes to stderr.
		var stderr strings.Builder
		c := &cobra.Command{}
		c.SetErr(&stderr)
		plain := errors.New("disk full")
		h := userRootHeal{ChmodGoW: []string{"/home/x"}}
		assert.Same(t, plain, h.explain(c, plain))
		assert.Equal(t, "capsulectl: install had already removed group and other write from \"/home/x\"\n", stderr.String())
		assert.NoError(t, h.explain(c, nil))
	})
	t.Run("the launcher fails", func(t *testing.T) {
		home, _ := installHome(t)
		require.NoError(t, os.Chmod(home, 0o775))
		bad := filepath.Join(t.TempDir(), "broken")
		require.NoError(t, os.WriteFile(bad, []byte("#!/bin/sh\nexit 1\n"), 0o755))
		_, err := invoke(t, "", "plugin", "install", bad)
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "install had already removed group and other write from "+strconv.Quote(resolved(t, home)))
		assert.Equal(t, uint32(0o755), modeOf(t, home))
	})
}

// resolved is path with its symlinks resolved, as the heal reports it.
func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return r
}

func TestPluginInstallRefusesWhatTheHealRefuses(t *testing.T) {
	t.Run("sticky home", func(t *testing.T) {
		home, root := installHome(t)
		require.NoError(t, unix.Chmod(home, 0o1775))
		_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "group-writable")
		assert.Contains(t, SafeError(err), "not healed: "+resolved(t, home)+" is sticky")
		assert.NotContains(t, SafeError(err), "already removed")
		assert.Equal(t, uint32(0o1775), modeOf(t, home))
		assert.NoDirExists(t, root)
	})
	t.Run("outside HOME", func(t *testing.T) {
		// ~/.local links into a writable directory outside HOME; a writable
		// ~ alongside it is left as it is too: if any part cannot be
		// healed, the heal changes none.
		home, root := installHome(t)
		outside := filepath.Join(safeTempDir(t), "shared")
		require.NoError(t, os.MkdirAll(filepath.Join(outside, "local"), 0o755))
		require.NoError(t, os.Chmod(outside, 0o777))
		require.NoError(t, os.Symlink(filepath.Join(outside, "local"), filepath.Join(home, ".local")))
		require.NoError(t, os.Chmod(home, 0o775))
		_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "writable")
		assert.Contains(t, SafeError(err), "is outside HOME")
		assert.Equal(t, uint32(0o777), modeOf(t, outside))
		assert.Equal(t, uint32(0o775), modeOf(t, home))
		assert.NoDirExists(t, root)
	})
	t.Run("as root, a shallow HOME (the shared heal's guard)", func(t *testing.T) {
		home, _ := installHome(t)
		require.NoError(t, os.Chmod(home, 0o775))
		seam(t, &healProcess, func() (int, int, []int) { return 0, 0, nil })
		real, err := filepath.EvalSymlinks(home)
		require.NoError(t, err)
		// /usr is one level below / on every platform the tests run on.
		h := healProblems("/usr", fullPathCheck(real), installPathHealedReason)
		assert.Equal(t, "refused", h.State)
		assert.Contains(t, h.Error, "at least two levels below /")
		assert.Equal(t, uint32(0o775), modeOf(t, home))
	})
}

// handInstalled places a launcher named acme in the per-user root under
// HOME, as a user installing by hand does, creating the root's missing
// directories 0755. It returns the launcher's path.
func handInstalled(t *testing.T, home string) string {
	t.Helper()
	root := userPluginRoot(home)
	require.NoError(t, os.MkdirAll(root, 0o755))
	path := filepath.Join(root, "capsulectl-acme")
	require.NoError(t, os.WriteFile(path, []byte(launcherScript("acme", "1", "")), 0o755))
	require.NoError(t, os.Chmod(path, 0o755))
	return path
}

func TestDispatchChecksThePerUserRootsAncestry(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string) // makes the ancestry unsafe
		dir   func(home string) string        // the directory the refusal names
		want  string
	}{
		{
			// Stock Ubuntu: umask 002 makes ~/.local 0775, owned by the
			// user's own group. That group may have other members, and no
			// name or gid proves it does not, so it is not exempt.
			name: "ubuntu 0775 ~/.local in the user's own group",
			setup: func(t *testing.T, home string) {
				local := filepath.Join(home, ".local")
				require.NoError(t, os.Chown(local, -1, os.Getgid()))
				require.NoError(t, os.Chmod(local, 0o775))
			},
			dir:  func(home string) string { return filepath.Join(home, ".local") },
			want: "is group-writable",
		},
		{
			name: "other-writable ~/.local/lib",
			setup: func(t *testing.T, home string) {
				require.NoError(t, os.Chmod(filepath.Join(home, ".local", "lib"), 0o757))
			},
			dir:  func(home string) string { return filepath.Join(home, ".local", "lib") },
			want: "is other-writable",
		},
		{
			name:  "group-writable HOME",
			setup: func(t *testing.T, home string) { require.NoError(t, os.Chmod(home, 0o775)) },
			dir:   func(home string) string { return home },
			want:  "is group-writable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := installHome(t)
			launcher := handInstalled(t, home)
			require.NoError(t, VerifyTrustedPath(launcher), "a safe ancestry is accepted")
			tc.setup(t, home)
			err := VerifyTrustedPath(launcher)
			require.ErrorIs(t, err, errWritable)
			dir, rerr := filepath.EvalSymlinks(tc.dir(home))
			require.NoError(t, rerr)
			assert.Contains(t, err.Error(), dir+" "+tc.want)
			assert.Contains(t, err.Error(), "chmod go-w "+shellQuote(dir), "the refusal names the fix")
			assert.Empty(t, discoverPlugins(), "discovery refuses it")
		})
	}
}

func TestDispatchJudgesASymlinkedLocalByItsTarget(t *testing.T) {
	// ~/.local symlinked into a safe directory is accepted.
	home, _ := installHome(t)
	safe := filepath.Join(safeTempDir(t), "local")
	require.NoError(t, os.Mkdir(safe, 0o755))
	require.NoError(t, os.Symlink(safe, filepath.Join(home, ".local")))
	require.NoError(t, VerifyTrustedPath(handInstalled(t, home)))

	// Into a shared directory another user can write: the walk resolves the
	// root into it, so the target's parent is checked, though it is outside
	// HOME.
	home, _ = installHome(t)
	shared := filepath.Join(safeTempDir(t), "shared")
	require.NoError(t, os.Mkdir(shared, 0o755))
	target := filepath.Join(shared, "local")
	require.NoError(t, os.Mkdir(target, 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(home, ".local")))
	launcher := handInstalled(t, home)
	require.NoError(t, os.Chmod(shared, 0o777))
	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), filepath.Base(shared)+" is group-writable")
}

func TestDispatchChecksTheDirectoryHoldingEachSymlink(t *testing.T) {
	// ~/.local leads into a writable directory, and ~/.local/lib leads on
	// into a safe one. The resolved root's ancestry never passes the
	// writable directory, but whoever can write it can replace the lib
	// symlink, so the root is refused at dispatch and at install.
	home, _ := installHome(t)
	writable := filepath.Join(safeTempDir(t), "writable")
	require.NoError(t, os.MkdirAll(filepath.Join(writable, "local"), 0o755))
	safeLib := filepath.Join(safeTempDir(t), "lib")
	require.NoError(t, os.Mkdir(safeLib, 0o755))
	require.NoError(t, os.Symlink(safeLib, filepath.Join(writable, "local", "lib")))
	require.NoError(t, os.Symlink(filepath.Join(writable, "local"), filepath.Join(home, ".local")))
	launcher := handInstalled(t, home)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(writable, 0o775))

	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), filepath.Base(writable)+" is group-writable")
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), filepath.Base(writable)+" is group-writable")
}

func TestDispatchChecksASymlinkedRoot(t *testing.T) {
	// The plugins directory itself links into a shared directory: the
	// launcher walk stops at the target, so its parent is the ancestry
	// check's.
	home, _ := installHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".local", "lib", "capsulectl"), 0o755))
	shared := filepath.Join(safeTempDir(t), "shared")
	require.NoError(t, os.MkdirAll(filepath.Join(shared, "plugins"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(shared, "plugins"), userPluginRoot(home)))
	launcher := handInstalled(t, home)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(shared, 0o777))
	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), filepath.Base(shared)+" is group-writable")
}

func TestDispatchChecksEveryHopOfASymlinkChain(t *testing.T) {
	// ~/.local -> shared/link -> safe/local: every resolved directory is
	// under safe, but whoever writes shared can repoint link.
	home, _ := installHome(t)
	shared := filepath.Join(safeTempDir(t), "shared")
	require.NoError(t, os.Mkdir(shared, 0o755))
	safe := filepath.Join(safeTempDir(t), "local")
	require.NoError(t, os.Mkdir(safe, 0o755))
	require.NoError(t, os.Symlink(safe, filepath.Join(shared, "link")))
	require.NoError(t, os.Symlink(filepath.Join(shared, "link"), filepath.Join(home, ".local")))
	launcher := handInstalled(t, home)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(shared, 0o775))

	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), filepath.Base(shared)+" is group-writable")
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), filepath.Base(shared)+" is group-writable")

	// A relative hop is followed from the directory holding it.
	require.NoError(t, os.Chmod(shared, 0o755))
	require.NoError(t, os.Remove(filepath.Join(home, ".local")))
	rel, err := filepath.Rel(home, filepath.Join(shared, "link"))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(rel, filepath.Join(home, ".local")))
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(shared, 0o775))
	assert.ErrorIs(t, VerifyTrustedPath(launcher), errWritable)
}

func TestLookupDirsRefusesASymlinkLoop(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Symlink("b", filepath.Join(dir, "a")))
	require.NoError(t, os.Symlink("a", filepath.Join(dir, "b")))
	hops := 0
	var links []string
	_, _, err = lookupDirs(dir, "a", &hops, &links)
	assert.ErrorContains(t, err, "too many levels of symbolic links")
}

func TestAncestryRefusesAnotherUsersSymlink(t *testing.T) {
	// A link in a sticky directory (such as /tmp) is its owner's to
	// repoint. A test cannot create a link another user owns, so the
	// user's own link is judged as if by another uid.
	home, _ := installHome(t)
	safe := filepath.Join(safeTempDir(t), "local")
	require.NoError(t, os.Mkdir(safe, 0o755))
	link := filepath.Join(home, ".local")
	require.NoError(t, os.Symlink(safe, link))
	handInstalled(t, home)
	real, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	hops := 0
	var links []string
	_, _, err = lookupDirs(real, filepath.Join(".local", "lib", "capsulectl", "plugins"), &hops, &links)
	require.NoError(t, err)
	// links also holds root-owned system links in the target's path
	// (/var on macOS).
	require.Contains(t, links, filepath.Join(real, ".local"), "the followed link is recorded")
	assert.Empty(t, linkOwnerProblems(links, os.Getuid()))
	if os.Getuid() == 0 {
		return // root owns every link and is trusted
	}
	problems := linkOwnerProblems([]string{filepath.Join(real, ".local")}, os.Getuid()+1)
	require.Len(t, problems, 1)
	assert.True(t, problems[0].owner)
	assert.ErrorIs(t, problems[0].err, errNotTrustedOwner)
	assert.Contains(t, unsafePathMessage("the plugin root", problems), "chown")
}

func TestDispatchChecksAUserRootLinkedIntoAnotherRoot(t *testing.T) {
	// The per-user root links into a subdirectory of an earlier root. The
	// launcher resolves under that root, but it was found in the per-user
	// one, so the per-user ancestry still decides.
	home, user := installHome(t)
	system := filepath.Join(safeTempDir(t), "system")
	require.NoError(t, os.MkdirAll(filepath.Join(system, "sub"), 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", system+string(os.PathListSeparator)+user)
	require.NoError(t, os.MkdirAll(filepath.Dir(user), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(system, "sub"), user))
	launcher := handInstalled(t, home)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))
	assert.ErrorIs(t, VerifyTrustedPath(launcher), errWritable)
}

func TestAncestryNamesAnUnresolvablePathWithoutAChmod(t *testing.T) {
	home, _ := installHome(t)
	lib := filepath.Join(home, ".local", "lib")
	require.NoError(t, os.MkdirAll(lib, 0o755))
	require.NoError(t, os.Symlink("capsulectl", filepath.Join(lib, "capsulectl")))
	problems := userRootAncestryProblems(home)
	require.Len(t, problems, 1)
	assert.True(t, problems[0].unresolved)
	msg := unsafePathMessage("the plugin root", problems)
	assert.Contains(t, msg, "too many levels of symbolic links")
	assert.Contains(t, msg, "repair or remove the links on")
	assert.NotContains(t, msg, "chmod")
}

func TestAncestryFollowsDotDotAndASymlinkedHome(t *testing.T) {
	// ~/.local -> sub/../local resolves through sub's parent, which is
	// then checked too.
	home, _ := installHome(t)
	base := filepath.Join(safeTempDir(t), "base")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "sub"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(base, "local"), 0o755))
	// Not filepath.Join, which would clean the "..".
	require.NoError(t, os.Symlink(base+"/sub/../local", filepath.Join(home, ".local")))
	launcher := handInstalled(t, home)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(filepath.Join(base, "sub"), 0o775))
	err := VerifyTrustedPath(launcher)
	require.ErrorIs(t, err, errWritable)
	assert.Contains(t, err.Error(), filepath.Join("base", "sub")+" is group-writable")

	// A symlinked HOME is judged from its target: its ~/.local is checked.
	real := safeTempDir(t)
	linked := filepath.Join(safeTempDir(t), "home")
	require.NoError(t, os.Symlink(real, linked))
	t.Setenv("HOME", linked)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", userPluginRoot(linked))
	launcher = handInstalled(t, linked)
	require.NoError(t, VerifyTrustedPath(launcher))
	require.NoError(t, os.Chmod(filepath.Join(real, ".local"), 0o775))
	assert.ErrorIs(t, VerifyTrustedPath(launcher), errWritable)
}

func TestAncestryErrorClasses(t *testing.T) {
	gone := unsafePathProblem{path: "/a", unresolved: true, err: errors.New("loop")}
	writable := unsafePathProblem{path: "/b", err: errors.New("group-writable")}
	owned := unsafePathProblem{path: "/c", owner: true, err: errors.New("owned")}
	assert.ErrorIs(t, newAncestryError("r", []unsafePathProblem{gone}), errOutsideRoots, "only unresolved")
	assert.ErrorIs(t, newAncestryError("r", []unsafePathProblem{gone, writable}), errWritable)
	assert.ErrorIs(t, newAncestryError("r", []unsafePathProblem{owned, writable}), errNotTrustedOwner)
	assert.ErrorIs(t, newAncestryError("r", []unsafePathProblem{writable, owned, gone}), errNotTrustedOwner)
	assert.Equal(t, []unsafePathProblem{{path: "rel", unresolved: true, err: errors.New("HOME=rel is not an absolute path")}}, userRootAncestryProblems("rel"))
}

func TestDispatchLeavesOtherRootsAncestryAlone(t *testing.T) {
	// The ancestry check is the per-user root's: a root
	// CAPSULECTL_PLUGIN_ROOTS names elsewhere keeps the walk's rule, which
	// stops at the root, even when HOME's own ancestry is unsafe.
	home, _ := installHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".local"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o777))
	parent := filepath.Join(safeTempDir(t), "parent")
	other := filepath.Join(parent, "plugins")
	require.NoError(t, os.MkdirAll(other, 0o755))
	launcher := filepath.Join(other, "capsulectl-acme")
	require.NoError(t, os.WriteFile(launcher, []byte(launcherScript("acme", "1", "")), 0o755))
	require.NoError(t, os.Chmod(launcher, 0o755))
	require.NoError(t, os.Chmod(parent, 0o777))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", other)
	assert.NoError(t, VerifyTrustedPath(launcher))
}

func TestPluginInstallJudgesASymlinkedLocalByItsTarget(t *testing.T) {
	home, _ := installHome(t)
	safe := filepath.Join(safeTempDir(t), "local")
	require.NoError(t, os.Mkdir(safe, 0o755))
	require.NoError(t, os.Symlink(safe, filepath.Join(home, ".local")))
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err, out)
	assert.True(t, strings.HasPrefix(installOut(t, out).Path, func() string { c, _ := filepath.EvalSymlinks(safe); return c }()), "the canonical path is reported")

	home2, _ := installHome(t)
	shared := filepath.Join(safeTempDir(t), "shared")
	require.NoError(t, os.Mkdir(shared, 0o755))
	target := filepath.Join(shared, "local")
	require.NoError(t, os.Mkdir(target, 0o755))
	require.NoError(t, os.Chmod(shared, 0o777))
	require.NoError(t, os.Symlink(target, filepath.Join(home2, ".local")))
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), filepath.Base(shared)+" is group-writable")
}

func TestPluginInstallAcceptsAStickyRootOwnedDirectory(t *testing.T) {
	info, err := os.Stat("/tmp")
	if err != nil || info.Mode()&os.ModeSticky == 0 {
		t.Skip("no sticky /tmp")
	}
	real, err := filepath.EvalSymlinks("/tmp")
	require.NoError(t, err)
	assert.Empty(t, fullPathCheck(real), "a root-owned sticky directory passes install's check")
	assert.Error(t, checkTrustedComponent(real, os.Getuid(), false), "the walk's strict rule still refuses it")
}

func TestPluginInstallShadowing(t *testing.T) {
	home, user := installHome(t)
	earlier := filepath.Join(home, "earlier")
	require.NoError(t, os.Mkdir(earlier, 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", earlier+string(os.PathListSeparator)+user)

	// A launcher the walk refuses does not shadow.
	require.NoError(t, os.WriteFile(filepath.Join(earlier, "capsulectl-acme"), []byte(launcherScript("acme", "0", "")), 0o775))
	require.NoError(t, os.Chmod(filepath.Join(earlier, "capsulectl-acme"), 0o775))
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err, out)

	// An accepted earlier launcher does: CONFLICT, destination unchanged.
	require.NoError(t, os.Chmod(filepath.Join(earlier, "capsulectl-acme"), 0o755))
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, SafeError(err), "would still be the one that runs")
	installed, _ := filepath.EvalSymlinks(filepath.Join(user, "capsulectl-acme"))
	meta, err := readPluginMetadata(installed, "acme")
	require.NoError(t, err)
	assert.Equal(t, "1", meta.Version, "the destination is unchanged")

	// A later root never shadows.
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", user+string(os.PathListSeparator)+earlier)
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "3", ""))
	require.NoError(t, err)

	// An earlier root on an unsafe path is a possible shadow and never runs.
	parent := filepath.Join(home, "unsafe")
	unsafeRoot := filepath.Join(parent, "plugins")
	require.NoError(t, os.MkdirAll(unsafeRoot, 0o755))
	marker := filepath.Join(t.TempDir(), "ran")
	require.NoError(t, os.WriteFile(filepath.Join(unsafeRoot, "capsulectl-acme"), []byte(launcherScript("acme", "0", marker)), 0o755))
	require.NoError(t, os.Chmod(parent, 0o775))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", unsafeRoot+string(os.PathListSeparator)+user)
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "4", ""))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, SafeError(err), "unverifiable launcher")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPluginInstallRootsOverrideMustNameTheUserRoot(t *testing.T) {
	installHome(t)
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", t.TempDir())
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "CAPSULECTL_PLUGIN_ROOTS does not include")
}

func TestPluginInstallRefusesANonRegularDestination(t *testing.T) {
	_, root := installHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "capsulectl-acme"), 0o755))
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "not a regular file")
	noLeftovers(t, root)
}

func TestPluginRemove(t *testing.T) {
	home, root := installHome(t)
	_, err := invoke(t, "", "plugin", "remove", "acme")
	require.ErrorIs(t, err, ErrInput, "nothing installed yet")

	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(root, "capsulectl-acme.d"), 0o755))
	later := filepath.Join(home, "later")
	require.NoError(t, os.Mkdir(later, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(later, "capsulectl-acme"), []byte(launcherScript("acme", "9", "")), 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root+string(os.PathListSeparator)+later)

	out, err := invoke(t, "", "plugin", "remove", "acme")
	require.NoError(t, err, out)
	var removed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &removed))
	assert.Contains(t, removed["kept"], "capsulectl-acme.d")
	assert.Contains(t, removed["now_selected"], filepath.Join("later", "capsulectl-acme"))
	_, err = os.Stat(filepath.Join(root, "capsulectl-acme"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	_, err = invoke(t, "", "plugin", "remove", "acme")
	require.ErrorIs(t, err, ErrInput)
	_, err = invoke(t, "", "plugin", "remove", "../x")
	require.ErrorIs(t, err, ErrInput)
}

func TestPluginLsListsAnInterruptedInstall(t *testing.T) {
	_, root := installHome(t)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, pluginInstallPrefix+"dead"), []byte("x"), 0o755))
	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, pluginInstallPrefix+"dead")
	assert.NotContains(t, out, `"name":"dead"`)
}

func TestReleaseTagPolicy(t *testing.T) {
	for _, v := range []string{"v0.2.0", "v0.1.0-rc16", "v1.0.0-beta.1", "v10.20.30"} {
		assert.True(t, releaseTagPolicy.MatchString(v), v)
	}
	for _, v := range []string{"0.1.0-dev", "v1.2", "v1.0.0-", "v1.0.0-rc..1", "v1.0.0+build"} {
		assert.False(t, releaseTagPolicy.MatchString(v), v)
	}
}

// release.yml enforces the same tag policy the core uses to decide it is a
// release build.
func TestReleaseWorkflowEnforcesTheTagPolicy(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	require.NoError(t, err)
	policy := strings.TrimSuffix(strings.TrimPrefix(releaseTagPolicy.String(), "^"), "$")
	assert.Contains(t, string(workflow), "grep -Eqx '"+policy+"'")
}

func TestMissingPluginSaysHowToInstall(t *testing.T) {
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	for _, version := range []string{"v0.2.0", "0.1.0-dev"} {
		CLIVersion = version
		assert.Contains(t, SafeError(missingPluginError("deal", nil)), "capsulectl plugin install ./capsulectl-deal")
		assert.NotContains(t, SafeError(missingPluginError("deal", nil)), "https://", "the core names no release location")
	}
}

func TestDispatchWarnsOnAMismatchedFirstPartyPlugin(t *testing.T) {
	_, root := installHome(t)
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	CLIVersion = "v0.2.0"
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-deal"), []byte(launcherScript("deal", "v0.1.0", "")), 0o755))
	out, err := invoke(t, "", "deal", "list")
	require.NoError(t, err, out)
	assert.Contains(t, out, "warning:")
	assert.Contains(t, out, "run: capsulectl plugin install ./capsulectl-deal")
	assert.Contains(t, out, "ran deal: list", "the command still runs")
}

// Replacing a launcher others could have changed never runs it.
func TestPluginInstallNeverRunsAnUnsafeOldLauncher(t *testing.T) {
	_, root := installHome(t)
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "ran")
	old := filepath.Join(root, "capsulectl-acme")
	require.NoError(t, os.WriteFile(old, []byte(launcherScript("acme", "0", marker)), 0o755))
	require.NoError(t, os.Chmod(old, 0o757))
	out, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "2", ""))
	require.NoError(t, err, out)
	assert.Contains(t, installOut(t, out).ReplacedVersion, "not inspected")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the old launcher never ran")
}

// Concurrent first installs both create, or accept, the same root.
func TestInstallRootToleratesAConcurrentCreate(t *testing.T) {
	home, root := installHome(t)
	errs := make(chan error, 8)
	for range 8 {
		go func() { _, err := installRoot(home, true); errs <- err }()
	}
	for range 8 {
		require.NoError(t, <-errs)
	}
	info, err := os.Stat(root)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// A refused install or remove runs no installed plugin: discovery is skipped
// for them, even with a flag before the subcommand.
func TestRefusedPluginManagementRunsNoInstalledPlugin(t *testing.T) {
	_, root := installHome(t)
	require.NoError(t, os.MkdirAll(root, 0o755))
	marker := filepath.Join(t.TempDir(), "ran")
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-acme"), []byte(launcherScript("acme", "1", marker)), 0o755))
	t.Setenv("SUDO_UID", "501")
	_, err := invoke(t, "", "--profile", "p", "plugin", "install", "./x")
	require.ErrorIs(t, err, ErrInput)
	_, err = invoke(t, "", "plugin", "remove", "acme")
	require.ErrorIs(t, err, ErrInput)
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no plugin handshake ran")

	// The control: any other command discovers, and so runs, the launcher.
	_, err = invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	_, statErr = os.Stat(marker)
	assert.NoError(t, statErr, "the marker works")
}

func TestPluginInstallReportsAnUnsyncedCommit(t *testing.T) {
	_, root := installHome(t)
	old := syncPluginDir
	syncPluginDir = func(*os.Root) error { return fmt.Errorf("sync failed") }
	t.Cleanup(func() { syncPluginDir = old })
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, errPluginDurability)
	assert.Contains(t, SafeError(err), "was installed at")
	assert.Equal(t, 1, ExitCode(err))
	_, statErr := os.Stat(filepath.Join(root, "capsulectl-acme"))
	assert.NoError(t, statErr, "the rename committed")
	noLeftovers(t, root)
}

// Install works on the canonical root it checked: repointing a symlinked
// ~/.local afterwards does not move where the launcher lands.
func TestPluginInstallUsesTheCheckedCanonicalRoot(t *testing.T) {
	home, _ := installHome(t)
	first, second := filepath.Join(safeTempDir(t), "a"), filepath.Join(safeTempDir(t), "b")
	require.NoError(t, os.Mkdir(first, 0o755))
	require.NoError(t, os.Mkdir(second, 0o755))
	link := filepath.Join(home, ".local")
	require.NoError(t, os.Symlink(first, link))
	root, err := installRoot(home, true)
	require.NoError(t, err)
	r, err := os.OpenRoot(root)
	require.NoError(t, err)
	defer r.Close()
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(second, link))
	src := launcherScript("acme", "1", "")
	staged, err := stageInRoot(r, "", 0o755, func(w io.Writer) error { _, err := io.WriteString(w, src); return err })
	require.NoError(t, err)
	_, err = finishInstall(r, root, staged, false)
	require.NoError(t, err)
	firstReal, _ := filepath.EvalSymlinks(first)
	_, err = os.Stat(filepath.Join(firstReal, "lib", "capsulectl", "plugins", "capsulectl-acme"))
	assert.NoError(t, err, "the launcher lands under the checked target")
	_, err = os.Stat(filepath.Join(second, "lib"))
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing reaches the swapped-in target")
}

func TestPluginRemoveEdgeCases(t *testing.T) {
	home, root := installHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "capsulectl-dir"), 0o755))
	_, err := invoke(t, "", "plugin", "remove", "dir")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "not a regular file")

	// A later root on an unsafe path is reported, never run.
	_, err = invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	parent := filepath.Join(home, "unsafe")
	later := filepath.Join(parent, "plugins")
	require.NoError(t, os.MkdirAll(later, 0o755))
	marker := filepath.Join(t.TempDir(), "ran")
	require.NoError(t, os.WriteFile(filepath.Join(later, "capsulectl-acme"), []byte(launcherScript("acme", "9", marker)), 0o755))
	require.NoError(t, os.Chmod(parent, 0o777))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root+string(os.PathListSeparator)+later)
	out, err := invoke(t, "", "plugin", "remove", "acme")
	require.NoError(t, err, out)
	assert.Contains(t, out, "may be selected (not inspected")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestPluginInstallListsEveryUnsafeComponent(t *testing.T) {
	home, _ := installHome(t)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.MkdirAll(filepath.Join(local, "lib"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(local, "lib"), 0o757))
	require.NoError(t, os.Chmod(local, 0o775))
	// A sticky HOME is not healed, so install heals none and lists them all.
	require.NoError(t, unix.Chmod(home, 0o1775))
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "is group-writable")
	assert.Contains(t, msg, "is other-writable")
	assert.Regexp(t, `chmod go-w \S+ \S+\.local \S+\.local/lib`, msg, "one combined fix")
}

// A hand-over (here the transitional `compose` alias, through
// ForwardToPlugin) warns on drift too.
func TestHandOverWarnsOnAMismatchedFirstPartyPlugin(t *testing.T) {
	_, root := installHome(t)
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	CLIVersion = "v0.2.0"
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-deal"), []byte(launcherScript("deal", "v0.1.0", "")), 0o755))
	out, err := invoke(t, "", "compose")
	require.NoError(t, err, out)
	assert.Contains(t, out, "warning:")
	assert.Contains(t, out, "ran deal: compose")
}

func TestProfilePlugin(t *testing.T) {
	assert.Equal(t, DealPluginName, profilePlugin(Profile{Type: "sqlite", Namespace: "deal"}))
	assert.Equal(t, "ledger", profilePlugin(Profile{Type: "sqlite", Namespace: "ns", Log: "ledger"}))
	assert.Equal(t, "", profilePlugin(Profile{Type: "sqlite", Namespace: "capsule"}), "a plain sqlite profile needs no plugin")
	assert.Equal(t, "", profilePlugin(Profile{Type: "sqlite"}))
	assert.Equal(t, BookPluginName, profilePlugin(Profile{Type: "jsonl", LogID: "l", Log: LogBook}))
	assert.Equal(t, "", profilePlugin(Profile{Type: "jsonl", LogID: "l", Log: LogCLL}))
	assert.Equal(t, "", profilePlugin(Profile{Type: "mysql"}))
}

// A plugin installed before ~/.local turned group-writable (stock Ubuntu,
// umask 002) is refused at dispatch and listed refused by `plugin ls`, each
// with the exact chmod that fixes it (capsule-cli#270).
func TestARefusedUserRootNamesItsFixInLsAndAtDispatch(t *testing.T) {
	home, _ := installHome(t)
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Chmod(local, 0o775))
	realLocal, err := filepath.EvalSymlinks(local)
	require.NoError(t, err)
	fix := "chmod go-w " + shellQuote(realLocal)

	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err, out)
	var listed struct {
		Refused []struct{ Path, Reason string } `json:"refused"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed), out)
	require.Len(t, listed.Refused, 1, out)
	assert.Contains(t, listed.Refused[0].Reason, "is group-writable")
	assert.Contains(t, listed.Refused[0].Reason, fix)

	_, err = invoke(t, "", "acme")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "capsulectl-acme plugin at")
	assert.Contains(t, SafeError(err), fix, "dispatch names the fix, not \"unknown command\"")
}

// A first-party plugin that is installed but refused says so, with the fix,
// not that it is not installed.
func TestARefusedFirstPartyPluginIsNotCalledMissing(t *testing.T) {
	home, _ := installHome(t)
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, DealPluginName, "1", ""))
	require.NoError(t, err)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Chmod(local, 0o775))
	realLocal, err := filepath.EvalSymlinks(local)
	require.NoError(t, err)
	_, err = invoke(t, "", "deal", "list")
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "chmod go-w "+shellQuote(realLocal))
	assert.NotContains(t, msg, "not installed")
}

// A refused launcher's filename is chosen by whoever could write its
// folder: one that is not a plugin name gets no command, so it never reaches
// help, and a refusal's paths reach the terminal with control characters
// escaped.
func TestARefusedLaunchersNameNeverReachesTheTerminalRaw(t *testing.T) {
	home, root := installHome(t)
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	evil := "capsulectl-ev\x1b[31mil"
	require.NoError(t, os.WriteFile(filepath.Join(root, evil), []byte(launcherScript("evil", "1", "")), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))

	out, err := invoke(t, "", "--help")
	require.NoError(t, err)
	assert.NotContains(t, out, "\x1b", "no refused name in help")
	assert.Contains(t, out, "acme", "a refused plugin name is still listed")

	out, err = invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "\x1b", "plugin ls escapes it")
	assert.Contains(t, out, `\u001b[31mil`)

	assert.Equal(t, `a\x1bb\x0ac`, terminalSafe("a\x1bb\nc"))
	assert.Equal(t, `x\u202ey\u2028z`, terminalSafe("x\u202ey\u2028z"))
	assert.Equal(t, `a\U000e0001b`, terminalSafe("a\U000e0001b"))
	assert.Equal(t, "plain /path é", terminalSafe("plain /path é"))
}

// Refused launchers on a group-writable ~/.local never run, never take a
// name from an accepted launcher in a later root or from a core command, and
// a refused first-party book says it is refused, as deal does.
func TestRefusedLaunchersNeverRunOrShadow(t *testing.T) {
	home, root := installHome(t)
	marker := filepath.Join(t.TempDir(), "refused-ran")
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", marker))
	require.NoError(t, err)
	for _, name := range []string{"verify", BookPluginName} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-"+name), []byte(launcherScript(name, "1", marker)), 0o755))
	}
	require.NoError(t, os.Remove(marker), "install's own handshake ran it once")
	later := safeTempDir(t)
	require.NoError(t, os.Chmod(later, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(later, "capsulectl-acme"), []byte(launcherScript("acme", "2", "")), 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root+string(os.PathListSeparator)+later)
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))

	out, err := invoke(t, "", "acme", "hello")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran acme: hello", "the accepted launcher in the later root runs")

	out, err = invoke(t, "", "verify", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--bundle", "the core verify keeps its name")

	_, err = invoke(t, "", "book", "close")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "capsulectl-book plugin at")
	assert.Contains(t, SafeError(err), "chmod go-w ")
	assert.NotContains(t, SafeError(err), "not installed")

	_, err = os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist, "no refused launcher ran")
}

// A refusal's path is escaped at dispatch too: a plugin name that passes the
// name check, on a root whose own path carries an escape sequence and that
// another user can write.
func TestARefusalsPathsAreEscapedAtDispatch(t *testing.T) {
	installHome(t)
	parent := safeTempDir(t)
	root := filepath.Join(parent, "r\x1b[31m")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-acme"), []byte(launcherScript("acme", "1", "")), 0o755))
	require.NoError(t, os.Chmod(root, 0o775))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	_, err := invoke(t, "", "acme")
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "capsulectl-acme plugin at")
	assert.NotContains(t, msg, "\x1b[", "no raw escape reaches the terminal")
	assert.Contains(t, msg, `r\x1b[31m`)
}

// The command tree holds one command per name: a refused launcher adds none
// for a name an accepted launcher or a core command holds, and two roots
// refusing the same name add one. A refused deal reached through an alias
// (compose) says it is refused, and an error runs no accepted launcher's
// handshake again.
func TestRefusedLaunchersAddOneCommandPerName(t *testing.T) {
	home, root := installHome(t)
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	for _, name := range []string{"verify", DealPluginName, "zeta"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-"+name), []byte(launcherScript(name, "1", "")), 0o755))
	}
	second := filepath.Join(safeTempDir(t), "second")
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(second, "capsulectl-zeta"), []byte(launcherScript("zeta", "1", "")), 0o755))
	require.NoError(t, os.Chmod(second, 0o775))
	later := safeTempDir(t)
	require.NoError(t, os.Chmod(later, 0o755))
	handshakes := filepath.Join(t.TempDir(), "handshakes")
	require.NoError(t, os.WriteFile(filepath.Join(later, "capsulectl-acme"), []byte(launcherScript("acme", "2", handshakes)), 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", strings.Join([]string{root, second, later}, string(os.PathListSeparator)))
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))

	counts := map[string]int{}
	for _, c := range NewCommand().Commands() {
		counts[c.Name()]++
		if c.Name() == DealPluginName {
			assert.True(t, strings.HasPrefix(c.Short, "Deals:"), "deal keeps its first-party placeholder: %s", c.Short)
			assert.Contains(t, c.Short, "refused: see capsulectl plugin ls")
			assert.NotContains(t, c.Short, "not installed")
		}
	}
	for _, name := range []string{"acme", "verify", "zeta", DealPluginName} {
		assert.Equal(t, 1, counts[name], name)
	}
	require.NoError(t, os.Remove(handshakes), "building the tree ran the accepted acme's handshake")

	_, err = invoke(t, "", "compose", "--member", "x")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "capsulectl-deal plugin at")
	assert.Contains(t, SafeError(err), "chmod go-w ")

	runs := func() int {
		raw, err := os.ReadFile(handshakes)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(raw), "ran")
	}
	require.NoError(t, os.Remove(handshakes))
	_, err = invoke(t, "", "deal", "list")
	require.ErrorIs(t, err, ErrInput)
	assert.Equal(t, 1, runs(), "the tree's discovery only: the error runs no handshake again")
}

// The printed fix is one command a shell runs as is: with a $HOME holding a
// space, `plugin ls` and dispatch print the same quoted chmod, and running it
// lets the plugin run again.
func TestThePrintedFixRunsAsPastedWithASpaceInHome(t *testing.T) {
	home := filepath.Join(safeTempDir(t), "my home")
	require.NoError(t, os.Mkdir(home, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("SUDO_UID", "")
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", userPluginRoot(home))
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.NoError(t, err)
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))
	realLocal, err := filepath.EvalSymlinks(filepath.Join(home, ".local"))
	require.NoError(t, err)
	want := "chmod go-w '" + realLocal + "'"

	out, err := invoke(t, "", "plugin", "ls")
	require.NoError(t, err)
	var listed struct {
		Refused []struct{ Reason string } `json:"refused"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed), out)
	require.Len(t, listed.Refused, 1)
	assert.Contains(t, listed.Refused[0].Reason, want)
	_, err = invoke(t, "", "acme")
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, want, "dispatch prints the same fix")

	fix := msg[strings.Index(msg, "fix: ")+len("fix: "):]
	require.NoError(t, exec.Command("/bin/sh", "-c", fix).Run(), fix)
	out, err = invoke(t, "", "acme", "again")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ran acme: again")

	assert.Equal(t, "/plain/path-1.x", shellQuote("/plain/path-1.x"))
	assert.Equal(t, `'/it'\''s here'`, shellQuote("/it's here"))
	assert.Equal(t, "''", shellQuote(""))
	for _, s := range []string{"/a$b", "/a*b", "/~a", "/a`b`", "/a\"b"} {
		assert.Equal(t, "'"+s+"'", shellQuote(s), "a shell would expand or split %s", s)
	}
}

// A plugin released on its own carries its own tag as version and the
// capsulectl release it embeds as core_version; the first-party check
// compares core_version (docs/design/326-plugins-leave-the-core.md, 4.2).
func TestPluginInstallFirstPartyCoreVersion(t *testing.T) {
	installHome(t)
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	CLIVersion = "v0.2.0"
	launcher := func(core string) string {
		path := filepath.Join(t.TempDir(), "capsulectl-deal")
		meta := fmt.Sprintf(`{"name":"deal","vendor":"t","version":"v1.4.0","core_version":%q,"plugin_api":"cli-plugin/v1"}`, core)
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nif [ \"$1\" = cli-plugin-metadata ]; then echo '"+meta+"'; exit 0; fi\n"), 0o755))
		return path
	}

	out, err := invoke(t, "", "plugin", "install", launcher("v0.2.0"))
	require.NoError(t, err, out)
	r := installOut(t, out)
	assert.Equal(t, "matches capsulectl v0.2.0", r.VersionCheck)
	assert.Equal(t, "v1.4.0", r.Version, "the result reports the plugin's own version")

	_, err = invoke(t, "", "plugin", "install", launcher("v0.1.9"))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, SafeError(err), "built for capsulectl v0.1.9")
}

func TestPluginHandshakeReportsCoreVersion(t *testing.T) {
	oldVersion, oldPlugin, oldCommit := CLIVersion, PluginVersion, PluginCommit
	t.Cleanup(func() { CLIVersion, PluginVersion, PluginCommit = oldVersion, oldPlugin, oldCommit })
	handshake := func() pluginInfo {
		root := NewPluginCommand(PluginMetadata{Name: "deal", Vendor: "t"})
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetArgs([]string{"cli-plugin-metadata"})
		require.NoError(t, root.Execute())
		var info pluginInfo
		require.NoError(t, json.Unmarshal(buf.Bytes(), &info), buf.String())
		return info
	}

	// Built inside capsule-cli: no plugin version, so both fields are the core's.
	CLIVersion, PluginVersion, PluginCommit = "v0.2.0", "", ""
	info := handshake()
	assert.Equal(t, "v0.2.0", info.Version)
	assert.Equal(t, "v0.2.0", info.CoreVersion)

	// Released on its own: its own tag, and the core it embeds.
	PluginVersion, PluginCommit = "v1.4.0", "abc1234"
	info = handshake()
	assert.Equal(t, "v1.4.0", info.Version)
	assert.Equal(t, "v0.2.0", info.CoreVersion)
	assert.Equal(t, "v0.2.0", info.coreVersion())
}

// The dispatch warning and doctor's plugin check compare core_version too,
// never a plugin's own release tag.
func TestDriftWarningAndDoctorCompareCoreVersion(t *testing.T) {
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	CLIVersion = "v0.2.0"

	assert.Empty(t, driftWarning(pluginInfo{Name: "deal", Version: "v1.4.0", CoreVersion: "v0.2.0", path: "/x/capsulectl-deal"}))
	assert.Empty(t, driftWarning(pluginInfo{Name: "deal", Version: "v0.2.0", path: "/x/capsulectl-deal"}), "no core_version: version is the core's")
	assert.Contains(t, driftWarning(pluginInfo{Name: "deal", Version: "v1.4.0", CoreVersion: "v0.1.9", path: "/x/capsulectl-deal"}), "is built for capsulectl v0.1.9, this capsulectl is v0.2.0")

	root := pluginRoot(t)
	launcher := func(core string) {
		meta := fmt.Sprintf(`{"name":"deal","vendor":"t","version":"v1.4.0","core_version":%q,"plugin_api":"cli-plugin/v1"}`, core)
		writeLauncher(t, root, "capsulectl-deal", "#!/bin/sh\nif [ \"$1\" = cli-plugin-metadata ]; then echo '"+meta+"'; exit 0; fi\n", 0o755)
	}
	launcher("v0.2.0")
	check, issue := pluginInstallCheck("deal")
	assert.Empty(t, issue)
	assert.Equal(t, "matches", check["version_check"])
	assert.Equal(t, "v1.4.0", check["version"], "doctor reports the plugin's own version")

	launcher("v0.1.9")
	check, issue = pluginInstallCheck("deal")
	assert.Equal(t, "mismatch", check["version_check"])
	assert.Contains(t, issue, "is built for capsulectl v0.1.9, this capsulectl is v0.2.0")
}

// The first-party plugins ship from their own releases (capsule-cli#326):
// installing one by name is refused, with the file form, on any core.
func TestPluginInstallByNameIsRefused(t *testing.T) {
	_, root := installHome(t)
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	for _, version := range []string{"v0.2.0", "0.1.0-dev"} {
		CLIVersion = version
		for _, name := range []string{"book", "deal"} {
			_, err := invoke(t, "", "plugin", "install", name)
			require.ErrorIs(t, err, ErrInput)
			assert.Contains(t, SafeError(err), "capsulectl plugin install ./capsulectl-"+name)
			assert.NotContains(t, SafeError(err), "https://", "the core names no release location")
		}
	}
	noLeftovers(t, root)
}
