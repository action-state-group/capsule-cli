package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// file install: on a development build the name form says to build it.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deal"), []byte(launcherScript("deal", "x", "")), 0o755))
	t.Chdir(dir)
	_, err = invoke(t, "", "plugin", "install", "deal")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "development build")
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
	home, root := installHome(t)
	local := filepath.Join(home, ".local")
	require.NoError(t, os.MkdirAll(filepath.Join(local, "lib"), 0o755))
	require.NoError(t, os.Chmod(local, 0o775))
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", marker))
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "group-writable")
	assert.Contains(t, SafeError(err), "chmod go-w")
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing ran on an unsafe path")
	_, statErr = os.Stat(root)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing was created")

	// Dispatch refuses a plugin placed by hand under the same 0775 ~/.local
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
			assert.Contains(t, err.Error(), "chmod go-w "+dir, "the refusal names the fix")
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
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	require.NoError(t, err)
	policy := strings.TrimSuffix(strings.TrimPrefix(releaseTagPolicy.String(), "^"), "$")
	assert.Contains(t, string(workflow), "grep -Eqx '"+policy+"'")
}

func TestMissingPluginSaysHowToInstall(t *testing.T) {
	old := CLIVersion
	t.Cleanup(func() { CLIVersion = old })
	CLIVersion = "v0.2.0"
	assert.Contains(t, SafeError(missingPluginError("deal")), "install it with: capsulectl plugin install deal")
	CLIVersion = "0.1.0-dev"
	assert.Contains(t, SafeError(missingPluginError("deal")), "capsulectl plugin install ./capsulectl-deal")
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
	assert.Contains(t, out, "run: capsulectl plugin install deal")
	assert.Contains(t, out, "ran deal: list", "the command still runs")
}

func TestSumFor(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	got, err := sumFor([]byte(a+"  x\n"+b+"  y\n"), "y")
	require.NoError(t, err)
	assert.Equal(t, b, got)
	for name, sums := range map[string]string{
		"missing":   a + "  x\n",
		"duplicate": b + "  y\n" + b + "  y\n",
		"malformed": "zz  y\n",
		"one space": b + " y\n",
	} {
		_, err := sumFor([]byte(sums), "y")
		assert.Error(t, err, name)
	}
}

// releaseServer serves a release of the deal plugin for version over TLS,
// and points the download client at it. files may override any path; a nil
// value makes that path a 404.
func releaseServer(t *testing.T, version string, files map[string][]byte) string {
	t.Helper()
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if !slices.Contains(releasePlatforms, platform) {
		t.Skip("no release build for " + platform)
	}
	asset := fmt.Sprintf("capsulectl-%s-plugin-deal-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	body := []byte(launcherScript("deal", version, ""))
	bundle := []byte(`{"bundle":true}`)
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	served := map[string][]byte{
		"/" + version + "/" + asset:                                 body,
		"/" + version + "/capsulectl-" + version + ".sigstore.json": bundle,
		"/" + version + "/SHA256SUMS":                               []byte(sum(body) + "  " + asset + "\n" + sum(bundle) + "  capsulectl-" + version + ".sigstore.json\n"),
	}
	for k, v := range files {
		if v == nil {
			delete(served, k) // a 404
			continue
		}
		served[k] = v
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-http" {
			http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
			return
		}
		data, ok := served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	oldTransport := pluginDownloadClient.Transport
	pluginDownloadClient.Transport = srv.Client().Transport
	t.Cleanup(func() { pluginDownloadClient.Transport = oldTransport })
	oldVersion := CLIVersion
	CLIVersion = version
	t.Cleanup(func() { CLIVersion = oldVersion })
	return srv.URL
}

// pathWithGh puts a fake gh on PATH that prints out and exits with code, or
// no gh at all when out is "".
func pathWithGh(t *testing.T, out string, code int) {
	t.Helper()
	dir := t.TempDir()
	if out != "" {
		script := fmt.Sprintf("#!/bin/sh\necho %q\nexit %d\n", out, code)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755))
	}
	t.Setenv("PATH", dir)
}

func TestPluginInstallByName(t *testing.T) {
	_, root := installHome(t)
	base := releaseServer(t, "v0.2.0", nil)
	pathWithGh(t, "", 0)
	out, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.NoError(t, err, out)
	r := installOut(t, out)
	assert.Equal(t, "deal", r.Name)
	assert.Equal(t, []string{"sha256 (SHA256SUMS)", "provenance: not checked (gh not installed)", "handshake (cli-plugin-metadata)"}, r.Checks)
	assert.Equal(t, "matches capsulectl v0.2.0", r.VersionCheck)
	noLeftovers(t, root)

	pathWithGh(t, "ok", 0)
	out, err = invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.NoError(t, err, out)
	assert.Contains(t, installOut(t, out).Checks, "provenance (gh attestation verify)")
	noLeftovers(t, root)

	// gh 2.102.0's own wording for a tampered artifact, a wrong --repo or a
	// wrong --signer-workflow: a failed check, with no way around it.
	pathWithGh(t, `Error: verifying with issuer "sigstore.dev"`, 1)
	_, err = invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, SafeError(err), "provenance check failed")
	assert.NotContains(t, SafeError(err), "without gh", "a failed check offers no bypass")

	pathWithGh(t, "Error: error creating Sigstore verifier: failed to fetch trust root", 1)
	_, err = invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.ErrorIs(t, err, errPluginFetch)
	assert.Contains(t, SafeError(err), "could not run")
	assert.Equal(t, 1, ExitCode(err))
	noLeftovers(t, root)
}

func TestPluginInstallByNameRefusals(t *testing.T) {
	_, root := installHome(t)
	pathWithGh(t, "", 0)
	asset := fmt.Sprintf("capsulectl-v0.2.0-plugin-deal-%s-%s", runtime.GOOS, runtime.GOARCH)
	for name, tc := range map[string]struct {
		files map[string][]byte
		class error
		says  string
	}{
		"hash mismatch":  {map[string][]byte{"/v0.2.0/" + asset: []byte("tampered")}, ErrConflict, "does not match its SHA256SUMS line"},
		"no sums line":   {map[string][]byte{"/v0.2.0/SHA256SUMS": []byte(strings.Repeat("a", 64) + "  other\n")}, ErrInput, "may not ship plugins"},
		"malformed sums": {map[string][]byte{"/v0.2.0/SHA256SUMS": []byte("not a sums file\n")}, ErrInput, "is not `<sha256>  <file>`"},
	} {
		t.Run(name, func(t *testing.T) {
			base := releaseServer(t, "v0.2.0", tc.files)
			_, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
			require.ErrorIs(t, err, tc.class)
			assert.Contains(t, SafeError(err), tc.says)
			noLeftovers(t, root)
			_, statErr := os.Stat(filepath.Join(root, "capsulectl-deal"))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	t.Run("no release", func(t *testing.T) {
		base := releaseServer(t, "v0.2.0", nil)
		CLIVersion = "v0.3.0"
		_, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
		require.ErrorIs(t, err, ErrInput)
		assert.Contains(t, SafeError(err), "has no SHA256SUMS")
	})
	t.Run("oversized", func(t *testing.T) {
		base := releaseServer(t, "v0.2.0", nil)
		old := maxPluginSums
		maxPluginSums = 10
		t.Cleanup(func() { maxPluginSums = old })
		_, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
		require.ErrorIs(t, err, errPluginFetch)
		assert.Contains(t, SafeError(err), "refusing to keep part of it")
	})
	t.Run("http redirect", func(t *testing.T) {
		base := releaseServer(t, "v0.2.0", nil)
		var sink strings.Builder
		_, err := downloadTo(t.Context(), base+"/redirect-http", &sink, 100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "downloads stay on HTTPS")
		_, err = downloadTo(t.Context(), "http://example.invalid/x", &sink, 100)
		assert.ErrorContains(t, err, "downloads use HTTPS")
	})
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
	_, err = finishInstall(r, root, staged, "", false, nil)
	require.NoError(t, err)
	firstReal, _ := filepath.EvalSymlinks(first)
	_, err = os.Stat(filepath.Join(firstReal, "lib", "capsulectl", "plugins", "capsulectl-acme"))
	assert.NoError(t, err, "the launcher lands under the checked target")
	_, err = os.Stat(filepath.Join(second, "lib"))
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing reaches the swapped-in target")
}

func TestPluginInstallByNameCapsTheAssetAndTheBundle(t *testing.T) {
	_, root := installHome(t)
	for name, cap := range map[string]*int64{"asset": &maxPluginAsset, "bundle": &maxPluginBundle} {
		t.Run(name, func(t *testing.T) {
			base := releaseServer(t, "v0.2.0", nil)
			pathWithGh(t, "ok", 0)
			old := *cap
			*cap = 8
			t.Cleanup(func() { *cap = old })
			_, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
			require.ErrorIs(t, err, errPluginFetch)
			assert.Contains(t, SafeError(err), "refusing to keep part of it")
			noLeftovers(t, root)
			_, statErr := os.Stat(filepath.Join(root, "capsulectl-deal"))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
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
	_, err := invoke(t, "", "plugin", "install", launcherFile(t, "acme", "1", ""))
	require.ErrorIs(t, err, ErrInput)
	msg := SafeError(err)
	assert.Contains(t, msg, "is group-writable")
	assert.Contains(t, msg, "is other-writable")
	assert.Regexp(t, `chmod go-w \S+\.local \S+\.local/lib`, msg, "one combined fix")
}

func TestPluginInstallByNameAssetMissingOrPlatform(t *testing.T) {
	_, root := installHome(t)
	pathWithGh(t, "", 0)
	asset := fmt.Sprintf("capsulectl-v0.2.0-plugin-deal-%s-%s", runtime.GOOS, runtime.GOARCH)
	base := releaseServer(t, "v0.2.0", map[string][]byte{"/v0.2.0/" + asset: nil})
	_, err := invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "has no asset "+asset)
	noLeftovers(t, root)

	old := releasePlatforms
	releasePlatforms = []string{"plan9/none"}
	t.Cleanup(func() { releasePlatforms = old })
	_, err = invoke(t, "", "plugin", "install", "--release-url", base, "deal")
	require.ErrorIs(t, err, ErrInput)
	assert.Contains(t, SafeError(err), "no release build of the plugins for "+runtime.GOOS+"/"+runtime.GOARCH)
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
	assert.Equal(t, DealPluginName, profilePlugin(Profile{Type: "sqlite"}))
	assert.Equal(t, BookPluginName, profilePlugin(Profile{Type: "jsonl", LogID: "l", Log: LogBook}))
	assert.Equal(t, "", profilePlugin(Profile{Type: "jsonl", LogID: "l", Log: LogCLL}))
	assert.Equal(t, "", profilePlugin(Profile{Type: "mysql"}))
}
