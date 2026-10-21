package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestMain gives the package a private HOME: doctor and the cadence heal the
// per-user plugin root's ancestry, and a test that sets no HOME of its own
// must never chmod the developer's real one.
func TestMain(m *testing.M) {
	keepGoCaches()
	home, err := os.MkdirTemp("", "capsulectl-home-")
	if err == nil {
		err = os.Chmod(home, 0o755)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// modeOf is path's permission, setgid and sticky bits, as chmod writes them.
func modeOf(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	return uint32(info.Sys().(*syscall.Stat_t).Mode) & 0o7777
}

// chmodOwnGroup sets mode on dir after giving it the process's own group, so
// a setgid bit survives the chmod on any platform.
func chmodOwnGroup(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chown(dir, -1, os.Getgid()))
	require.NoError(t, unix.Chmod(dir, uint32(mode)))
}

// dealLauncher is a capsulectl-deal that answers the handshake, records each
// run in marker, and then runs body.
func dealLauncher(marker, body string) string {
	return fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = cli-plugin-metadata ]; then echo '{\"name\":\"deal\",\"vendor\":\"t\",\"version\":\"1\",\"plugin_api\":\"cli-plugin/v1\"}'; exit 0; fi\necho ran >> %s\necho \"ran deal: $*\"\n%s\n", marker, body)
}

// dealHome is a private HOME with capsulectl-deal hand-installed in its
// per-user root. It returns HOME resolved, the root, and the run marker.
func dealHome(t *testing.T, body string) (string, string, string) {
	t.Helper()
	home, root := installHome(t)
	require.NoError(t, os.MkdirAll(root, 0o755))
	for p := root; p != home; p = filepath.Dir(p) {
		require.NoError(t, os.Chmod(p, 0o755))
	}
	marker := filepath.Join(safeTempDir(t), "ran")
	require.NoError(t, os.WriteFile(filepath.Join(root, "capsulectl-deal"), []byte(dealLauncher(marker, body)), 0o755))
	real, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return real, userPluginRoot(real), marker
}

func ran(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// seam replaces a test seam for one test.
func seam[T any](t *testing.T, v *T, with T) {
	old := *v
	*v = with
	t.Cleanup(func() { *v = old })
}

// cadenceLines runs `cll checkpoint cadence` and returns each JSON line of
// its output by state, and the rest of the output.
func cadenceLines(t *testing.T) (map[string]map[string]any, string, error) {
	t.Helper()
	out, err := invoke(t, "", "cll", "checkpoint", "cadence")
	lines := map[string]map[string]any{}
	var rest []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil {
			if s, ok := v["state"].(string); ok {
				lines[s] = v
				continue
			}
		}
		rest = append(rest, line)
	}
	return lines, strings.Join(rest, "\n"), err
}

func TestHealUserRootHealsAGroupWritableHome(t *testing.T) {
	home, root, _ := dealHome(t, "")
	chmodOwnGroup(t, home, 0o2770)
	require.NotEmpty(t, userRootAncestryProblems(home), "the walk refuses it")

	h := healUserRoot(home)
	require.Equal(t, "healed", h.State, h.Error)
	assert.Equal(t, []string{home}, h.ChmodGoW)
	assert.Equal(t, map[string][2]string{home: {"2770", "2750"}}, h.Modes)
	assert.Empty(t, h.SetgidDropped)
	assert.Equal(t, uint32(0o2750), modeOf(t, home), "write removed, setgid kept")
	assert.Empty(t, userRootAncestryProblems(home))
	assert.Equal(t, uint32(0o755), modeOf(t, root))

	assert.Equal(t, userRootHeal{State: "ok"}, healUserRoot(home), "a second run heals nothing")
}

func TestHealUserRootHealsEveryWritableDirectoryAtOnce(t *testing.T) {
	home, root, _ := dealHome(t, "")
	local := filepath.Join(home, ".local")
	for _, d := range []string{home, local, root} {
		require.NoError(t, os.Chmod(d, 0o775))
	}
	h := healUserRoot(home)
	require.Equal(t, "healed", h.State, h.Error)
	assert.ElementsMatch(t, []string{home, local, root}, h.ChmodGoW)
	for _, d := range []string{home, local, root} {
		assert.Equal(t, uint32(0o755), modeOf(t, d), d)
	}
}

func TestHealUserRootOwnership(t *testing.T) {
	home, _, _ := dealHome(t, "")

	// A root-owned directory under a non-root process is refused.
	require.NoError(t, os.Chmod(home, 0o775))
	seam(t, &healOwner, func(*unix.Stat_t) int { return 0 })
	h := healUserRoot(home)
	assert.Equal(t, "refused", h.State)
	assert.Contains(t, h.Error, "not owned by this process's user")
	assert.Contains(t, h.Error, "chmod go-w", "the refusal keeps the walk's fix")
	assert.Equal(t, uint32(0o775), modeOf(t, home))

	// Root-owned under a root process (the Muse case) heals.
	gid := os.Getgid()
	seam(t, &healProcess, func() (int, int, []int) { return 0, gid, nil })
	h = healUserRoot(home)
	require.Equal(t, "healed", h.State, h.Error)
	assert.Equal(t, uint32(0o755), modeOf(t, home))
}

func TestHomeDepthForRoot(t *testing.T) {
	assert.False(t, homeDeepEnoughForRoot("/tmp"))
	assert.False(t, homeDeepEnoughForRoot("/root"))
	assert.True(t, homeDeepEnoughForRoot("/home/hatch"))
}

func TestHealUserRootRefusesASharedHome(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, unix.Chmod(home, 0o1775))
	h := healUserRoot(home)
	assert.Equal(t, "refused", h.State)
	assert.Contains(t, h.Error, "sticky")
	assert.Equal(t, uint32(0o1775), modeOf(t, home))
}

func TestHealUserRootNeverLeavesHome(t *testing.T) {
	// ~/.local links into a writable directory outside HOME: the walk refuses
	// that directory, and the heal does not touch it.
	home, _ := installHome(t)
	outside := filepath.Join(safeTempDir(t), "shared")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "local", "lib", "capsulectl", "plugins"), 0o755))
	require.NoError(t, os.Chmod(outside, 0o777))
	require.NoError(t, os.Symlink(filepath.Join(outside, "local"), filepath.Join(home, ".local")))
	h := healUserRoot(home)
	assert.Equal(t, "refused", h.State)
	assert.Contains(t, h.Error, "outside HOME")
	assert.Equal(t, uint32(0o777), modeOf(t, outside))
}

func TestHealUserRootWithoutARoot(t *testing.T) {
	// A dangling ~/.local: nothing is gated, nothing heals.
	home, _ := installHome(t)
	require.NoError(t, os.Chmod(home, 0o775))
	require.NoError(t, os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, ".local")))
	assert.Equal(t, "absent", healUserRoot(home).State)
	assert.Equal(t, uint32(0o775), modeOf(t, home))

	// Stock Ubuntu with no per-user root yet.
	home, _ = installHome(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, ".local"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(home, ".local"), 0o775))
	assert.Equal(t, "absent", healUserRoot(home).State)
	assert.Equal(t, uint32(0o775), modeOf(t, filepath.Join(home, ".local")))

	// The per-user root is not a plugin root at all.
	home, _, _ = dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", safeTempDir(t))
	assert.Equal(t, "absent", healUserRoot(home).State)
	assert.Equal(t, uint32(0o775), modeOf(t, home))
}

// What preflight opens is judged, not what the ancestry check saw.
func TestHealUserRootJudgesWhatItOpens(t *testing.T) {
	cases := map[string]func(t *testing.T, home, outside string){
		// The exact swap a plain open or Lstat would follow out of HOME.
		"~/.local swapped for a link to the same layout outside HOME": func(t *testing.T, home, outside string) {
			local := filepath.Join(home, ".local")
			require.NoError(t, os.Rename(local, local+".orig"))
			require.NoError(t, os.Symlink(outside, local))
		},
		"~/.local swapped for a FIFO": func(t *testing.T, home, _ string) {
			local := filepath.Join(home, ".local")
			require.NoError(t, os.Rename(local, local+".orig"))
			require.NoError(t, unix.Mkfifo(local, 0o600))
		},
		"~/.local/lib swapped for a setgid directory of a group this process is not in": func(t *testing.T, home, _ string) {
			lib := filepath.Join(home, ".local", "lib")
			require.NoError(t, os.Rename(lib, lib+".orig"))
			require.NoError(t, os.Mkdir(lib, 0o755))
			chmodOwnGroup(t, lib, 0o2775)
		},
		"~/.local/lib swapped for a sticky directory": func(t *testing.T, home, _ string) {
			lib := filepath.Join(home, ".local", "lib")
			require.NoError(t, os.Rename(lib, lib+".orig"))
			require.NoError(t, os.Mkdir(lib, 0o755))
			require.NoError(t, unix.Chmod(lib, 0o1775))
		},
	}
	for name, swap := range cases {
		t.Run(name, func(t *testing.T) {
			home, _, _ := dealHome(t, "")
			lib := filepath.Join(home, ".local", "lib")
			require.NoError(t, os.Chmod(lib, 0o775))
			outside := filepath.Join(safeTempDir(t), "local")
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "lib", "capsulectl", "plugins"), 0o755))
			require.NoError(t, os.Chmod(filepath.Join(outside, "lib"), 0o775))
			// The process is in no group, so a setgid directory swapped in fails
			// the group rule; the other swaps fail before it applies.
			uid := os.Getuid()
			seam(t, &healProcess, func() (int, int, []int) { return uid, -1, nil })
			seam(t, &healBeforePreflight, func() { swap(t, home, outside) })

			h := healUserRoot(home)
			assert.Equal(t, "refused", h.State)
			assert.Equal(t, uint32(0o775), modeOf(t, filepath.Join(outside, "lib")), "nothing outside HOME changes")
			assert.Empty(t, h.ChmodGoW, "nothing changes")
		})
	}
}

func TestHealUserRootKeepsSetgid(t *testing.T) {
	home, _, _ := dealHome(t, "")
	chmodOwnGroup(t, home, 0o2770)
	// The directory's group is not one this non-root process is in.
	uid := os.Getuid()
	seam(t, &healProcess, func() (int, int, []int) { return uid, -1, nil })
	h := healUserRoot(home)
	assert.Equal(t, "refused", h.State)
	assert.Contains(t, h.Error, "setgid")
	assert.Equal(t, uint32(0o2770), modeOf(t, home))
}

func TestHealUserRootCountsAConcurrentHeal(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	seam(t, &healBeforePreflight, func() { require.NoError(t, os.Chmod(home, 0o755)) })
	assert.Equal(t, userRootHeal{State: "ok"}, healUserRoot(home))
}

func TestCadenceHealsAWritablePluginRootThenTicks(t *testing.T) {
	home, root, marker := dealHome(t, "")
	chmodOwnGroup(t, root, 0o2770)
	lines, rest, err := cadenceLines(t)
	require.NoError(t, err, rest)
	healed := lines["plugin_root_healed"]
	require.NotNil(t, healed, rest)
	assert.Equal(t, []any{root}, healed["chmod_go_w"])
	assert.Contains(t, healed["reason"], "VM replacement")
	assert.Contains(t, rest, "ran deal: tick")
	assert.True(t, ran(marker))
	assert.Equal(t, uint32(0o2750), modeOf(t, root))
	assert.Equal(t, uint32(0o755), modeOf(t, home))
}

func TestCadenceHealsAGroupWritableHome(t *testing.T) {
	home, root, marker := dealHome(t, "")
	// The plugin roots named canonically while HOME is not (on macOS the
	// temporary directory is under the /var link): still the per-user root.
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", root)
	chmodOwnGroup(t, home, 0o2770)
	lines, rest, err := cadenceLines(t)
	require.NoError(t, err, rest)
	require.NotNil(t, lines["plugin_root_healed"], rest)
	assert.True(t, ran(marker))
	assert.Equal(t, uint32(0o2750), modeOf(t, home))

	// Healed already: the next run writes no healed line.
	lines, rest, err = cadenceLines(t)
	require.NoError(t, err, rest)
	assert.Nil(t, lines["plugin_root_healed"])
}

func TestCadenceReportsAPartialHealAndDoesNotRun(t *testing.T) {
	home, _, marker := dealHome(t, "")
	local := filepath.Join(home, ".local")
	require.NoError(t, os.Chmod(home, 0o775))
	require.NoError(t, os.Chmod(local, 0o775))
	calls := 0
	seam(t, &healChmod, func(fd int, mode uint32) error {
		if calls++; calls == 2 {
			return errors.New("injected")
		}
		return unix.Fchmod(fd, mode)
	})
	lines, rest, err := cadenceLines(t)
	require.ErrorIs(t, err, ErrInput, rest)
	healed := lines["plugin_root_healed"]
	require.NotNil(t, healed, rest)
	assert.Len(t, healed["chmod_go_w"], 1)
	assert.Len(t, healed["failed"], 1)
	require.NotNil(t, lines["plugin_unavailable"], rest)
	assert.False(t, ran(marker), "the walk still refuses the chain")
	assert.Equal(t, uint32(0o755), modeOf(t, home), "the first stays tightened")
	assert.Equal(t, uint32(0o775), modeOf(t, local))
}

func TestCadenceLeavesAValidSystemPluginAlone(t *testing.T) {
	home, root, marker := dealHome(t, "")
	system := safeTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(system, "capsulectl-deal"), []byte(dealLauncher(marker, "")), 0o755))
	t.Setenv("CAPSULECTL_PLUGIN_ROOTS", system+string(os.PathListSeparator)+root)
	require.NoError(t, os.Chmod(home, 0o775))
	lines, rest, err := cadenceLines(t)
	require.NoError(t, err, rest)
	assert.Empty(t, lines, rest)
	assert.True(t, ran(marker))
	assert.Equal(t, uint32(0o775), modeOf(t, home), "no heal while a valid plugin runs")
}

func TestOtherForwardsNeverHeal(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	out, err := invoke(t, "", "compose")
	require.Error(t, err)
	assert.NotContains(t, out, "plugin_root_healed")
	assert.NotContains(t, out, "plugin_unavailable")
	assert.Equal(t, uint32(0o775), modeOf(t, home))
}

func TestCadenceSaysWhyThePluginDidNotRun(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		installHome(t)
		lines, rest, err := cadenceLines(t)
		require.ErrorIs(t, err, ErrInput)
		require.NotNil(t, lines["plugin_unavailable"], rest)
		assert.Equal(t, SafeError(err), lines["plugin_unavailable"]["error"], "the error is returned unchanged")
		assert.Contains(t, SafeError(err), "capsulectl-deal plugin, which is not installed")
	})
	t.Run("refused for a reason the heal cannot fix", func(t *testing.T) {
		_, root, marker := dealHome(t, "")
		require.NoError(t, os.Chmod(filepath.Join(root, "capsulectl-deal"), 0o775))
		lines, rest, err := cadenceLines(t)
		require.ErrorIs(t, err, ErrInput)
		assert.Nil(t, lines["plugin_root_healed"])
		require.NotNil(t, lines["plugin_unavailable"], rest)
		assert.Contains(t, lines["plugin_unavailable"]["error"], "is refused")
		assert.False(t, ran(marker))
	})
	t.Run("the plugin's own exit", func(t *testing.T) {
		dealHome(t, "exit 3")
		lines, rest, err := cadenceLines(t)
		var exit *PluginExit
		require.ErrorAs(t, err, &exit)
		assert.Empty(t, lines, rest)
	})
	t.Run("killed after it started", func(t *testing.T) {
		dealHome(t, "kill -9 $$")
		lines, rest, err := cadenceLines(t)
		require.ErrorIs(t, err, errPluginStopped)
		require.NotNil(t, lines["plugin_stopped"], rest)
		assert.Nil(t, lines["plugin_unavailable"])
	})
}

func doctorUserRootState(t *testing.T, args ...string) (map[string]any, map[string]any) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, _ := invoke(t, "", append([]string{"doctor"}, args...)...)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report), out)
	if plugins, ok := report["plugins"].(map[string]any); ok {
		ur, _ := plugins["user_root"].(map[string]any)
		return report, ur
	}
	report, _ = report["install_check"].(map[string]any)
	ur, _ := report["user_root"].(map[string]any)
	return report, ur
}

func TestDoctorReportsAndHealsTheUserRoot(t *testing.T) {
	home, _, _ := dealHome(t, "")
	_, ur := doctorUserRootState(t)
	assert.Equal(t, "ok", ur["state"])

	require.NoError(t, os.Chmod(home, 0o775))
	report, ur := doctorUserRootState(t)
	assert.Equal(t, "healed", ur["state"])
	assert.Equal(t, uint32(0o755), modeOf(t, home))
	discovered := report["plugins"].(map[string]any)["discovered"].([]any)
	assert.Len(t, discovered, 1, "the plugins listed are the ones that now load")

	seam(t, &healOwner, func(*unix.Stat_t) int { return -1 })
	require.NoError(t, os.Chmod(home, 0o775))
	_, ur = doctorUserRootState(t)
	assert.Equal(t, "refused", ur["state"])
	assert.Contains(t, ur["error"], "chmod go-w")

	installHome(t)
	_, ur = doctorUserRootState(t)
	assert.Equal(t, "absent", ur["state"])
}

func TestCadenceRunsAfterAConcurrentHeal(t *testing.T) {
	home, _, marker := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	// Another run tightens the home between this run's discovery and heal.
	seam(t, &healBeforePreflight, func() { require.NoError(t, os.Chmod(home, 0o755)) })
	lines, rest, err := cadenceLines(t)
	require.NoError(t, err, rest)
	assert.Empty(t, lines, rest)
	assert.True(t, ran(marker), "the tick runs on the healed chain")
}

func TestHealUserRootReportsAChmodItCouldNotVerify(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	seam(t, &healChmod, func(fd int, mode uint32) error { return unix.Fchmod(fd, mode|0o002) })
	h := healUserRoot(home)
	assert.Equal(t, "healed", h.State)
	assert.Equal(t, []string{home}, h.ChmodGoW, "the chmod happened, so it is reported")
	assert.Equal(t, [2]string{"775", "757"}, h.Modes[home])
	assert.Contains(t, h.Failed[home], "want 755")
}

func TestDoctorReportsWhatTheWalkSaysAfterHealing(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	local := filepath.Join(home, ".local")
	// A new problem appears on the chain after the check.
	seam(t, &healBeforePreflight, func() { require.NoError(t, os.Chmod(local, 0o775)) })
	_, ur := doctorUserRootState(t)
	assert.Equal(t, "refused", ur["state"])
	assert.Equal(t, []any{home}, ur["chmod_go_w"], "what it changed is still reported")
	assert.Contains(t, ur["error"], local)
	assert.Equal(t, uint32(0o755), modeOf(t, home))
}

func TestDoctorRechecksAfterAConcurrentHeal(t *testing.T) {
	home, _, _ := dealHome(t, "")
	require.NoError(t, os.Chmod(home, 0o775))
	local := filepath.Join(home, ".local")
	// Another run heals HOME first, and a new problem appears on the chain.
	seam(t, &healBeforePreflight, func() {
		require.NoError(t, os.Chmod(home, 0o755))
		require.NoError(t, os.Chmod(local, 0o775))
	})
	_, ur := doctorUserRootState(t)
	assert.Equal(t, "refused", ur["state"])
	assert.Contains(t, ur["error"], local)
}

// keepGoCaches pins the Go build and module caches to where they are under
// the real HOME before a test HOME replaces it: the tests that run `go build`
// would otherwise fill the test HOME with a fresh, read-only module cache.
func keepGoCaches() {
	if os.Getenv("GOPATH") == "" {
		if real, err := os.UserHomeDir(); err == nil {
			os.Setenv("GOPATH", filepath.Join(real, "go"))
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
		}
	}
}
