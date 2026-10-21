package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// pluginAPI is the handshake contract version. A plugin's cli-plugin-metadata
// must advertise exactly this; any other value is a loud dispatch failure rather
// than a silently-ignored plugin.
const PluginAPI = "cli-plugin/v1"

// pluginLauncherPrefix names a plugin launcher on a trusted path: capsulectl-<name>.
const pluginLauncherPrefix = "capsulectl-"

// trustedPluginRoots are the ONLY two directories a launcher may live in. Docker's
// plugin model is the reference: discovery scans fixed roots, never installs into
// the core, and refuses anything reachable through a world- or group-writable path.
func trustedPluginRoots() []string {
	if override := os.Getenv("CAPSULECTL_PLUGIN_ROOTS"); override != "" {
		return filepath.SplitList(override)
	}
	roots := []string{"/usr/local/lib/capsulectl/plugins"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".local", "lib", "capsulectl", "plugins"))
	}
	return roots
}

// The ways a path fails the trust walk, wrapped in its error so a caller can
// tell them apart with errors.Is.
var (
	errNotRegularFile  = errors.New("not a regular file")
	errOutsideRoots    = errors.New("outside the trusted plugin roots")
	errNotTrustedOwner = errors.New("owned by neither the current user nor root")
	errWritable        = errors.New("writable by others")
)

// verifyTrustedPath rejects a launcher whose real path, or any directory between
// it and the matched trusted root (inclusive), is group-writable, other-writable,
// or owned by neither the current user nor root; and rejects any launcher whose
// resolved path escapes the trusted roots (so a symlink cannot redirect discovery
// to an untrusted target) or is not a regular file. The system root's parents
// are system directories, trusted by definition. The per-user root's are not:
// for a path under it, by name or once resolved, userRootAncestryProblems
// also checks every directory from $HOME down.
func VerifyTrustedPath(path string) error {
	_, err := CheckedTrustedPath(path)
	return err
}

// CheckedTrustedPath is VerifyTrustedPath returning the resolved path the
// checks judged. A caller opens or executes that path, never path itself:
// a symlink on path swapped after the check then reaches nothing it runs.
func CheckedTrustedPath(path string) (string, error) {
	return checkedTrustedPathUnder(path, trustedPluginRoots())
}

// verifyTrustedPathUnder is verifyTrustedPath against the given roots: the
// path must resolve inside one of them, and the walk stops at that root.
func verifyTrustedPathUnder(path string, roots []string) error {
	_, err := checkedTrustedPathUnder(path, roots)
	return err
}

// checkedTrustedPathUnder is CheckedTrustedPath against the given roots.
func checkedTrustedPathUnder(path string, roots []string) (string, error) {
	// Absolute from the start, so a relative path or root (a relative
	// CAPSULECTL_PLUGIN_ROOTS entry) compares like for like below.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	if info, err := os.Lstat(resolved); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is %w", resolved, errNotRegularFile)
	}
	realRoot := ""
	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rr, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			continue
		}
		if resolved == rr || strings.HasPrefix(resolved, rr+string(os.PathSeparator)) {
			realRoot = rr
			break
		}
	}
	if realRoot == "" {
		return "", fmt.Errorf("%s resolves to %s, %w", path, resolved, errOutsideRoots)
	}
	// Walk from the launcher up to and including the trusted root: any directory
	// BETWEEN the root and the launcher (or the launcher itself) that is
	// group/other-writable or owned by neither the user nor root defeats the
	// trust. The per-user root's ancestry is checked after, for a path under
	// it by name or once resolved: a per-user root linked into the system
	// root is still the per-user root.
	uid := os.Getuid()
	for p := resolved; ; {
		if err := checkTrustedComponent(p, uid, false); err != nil {
			return "", err
		}
		if p == realRoot {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if problems := launcherHopProblems(path, resolved, realRoot, roots); len(problems) > 0 {
		return "", newAncestryError("the launcher "+path, problems)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && underUserRoot(path, resolved, userPluginRoot(home)) {
		if problems := userRootAncestryProblems(home); len(problems) > 0 {
			return "", newAncestryError("the plugin root "+userPluginRoot(home), problems)
		}
	}
	return resolved, nil
}

// insideHopRoots reports whether dir is in the root a launcher was found
// under (start) or the one it resolves into (realRoot): the hop check holds
// those to the strict rule and allows a root-owned sticky directory only
// outside both.
func insideHopRoots(dir, start, realRoot string) bool {
	in := func(root string) bool { return dir == root || strings.HasPrefix(dir, root+string(os.PathSeparator)) }
	return in(realRoot) || in(start)
}

// launcherHopProblems checks the symlink hops on path below the root it is
// found under. The walk above judges only the resolved path, so a directory
// that holds a hop but is not on it (R/capsulectl-x -> R/a/link -> R/b/x
// leaves R/a off it) is checked here: path is resolved again one name at a
// time from that root (lookupDirs), every directory a name is looked up in
// must pass checkTrustedComponent (strictly inside the root it was found
// under or resolves into; outside them a root-owned sticky directory is
// accepted, as for the per-user ancestry),
// and every link followed must be owned by the user or root. The lookup
// must also arrive at resolved. A directory above the root (an absolute
// link target passes through them) is left to the root's own rule: the
// system root's parents are trusted, and the per-user root's are judged by
// userRootAncestryProblems. A path under no root by name has no hops below
// one; its caller runs the resolved path all the same.
func launcherHopProblems(path, resolved, realRoot string, roots []string) []unsafePathProblem {
	abs, err := filepath.Abs(path)
	if err != nil {
		return []unsafePathProblem{{path: path, unresolved: true, err: err}}
	}
	under := func(p, dir string) bool { return strings.HasPrefix(p, dir+string(os.PathSeparator)) }
	start, rel := "", ""
	for _, root := range roots {
		lexical, err := filepath.Abs(root)
		if err != nil || !under(abs, lexical) {
			continue
		}
		if start, err = filepath.EvalSymlinks(lexical); err != nil {
			return []unsafePathProblem{{path: root, unresolved: true, err: fmt.Errorf("cannot resolve %s: %w", root, err)}}
		}
		rel, _ = filepath.Rel(lexical, abs)
		break
	}
	if start == "" {
		if !under(abs, realRoot) {
			return nil
		}
		start, _ = filepath.Rel(realRoot, abs)
		start, rel = realRoot, start
	}
	hops := 0
	var links []string
	got, dirs, err := lookupDirs(start, rel, &hops, &links)
	if err != nil {
		return []unsafePathProblem{{path: path, unresolved: true, err: fmt.Errorf("cannot resolve %s: %w", path, err)}}
	}
	if got != resolved {
		return []unsafePathProblem{{path: path, unresolved: true, err: fmt.Errorf("%s resolved to %s, then to %s: it changed during the check", path, resolved, got)}}
	}
	uid := os.Getuid()
	problems := linkOwnerProblems(links, uid)
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if under(realRoot, dir) || under(start, dir) {
			continue
		}
		if err := checkTrustedComponent(dir, uid, !insideHopRoots(dir, start, realRoot)); err != nil {
			problems = append(problems, unsafePathProblem{path: dir, owner: errors.Is(err, errNotTrustedOwner), err: err})
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].path < problems[j].path })
	return problems
}

// underUserRoot reports whether a launcher looked up as path, resolving to
// resolved, belongs to the per-user root userRoot: path names it under
// userRoot, or resolved lies under userRoot's own resolution.
func underUserRoot(path, resolved, userRoot string) bool {
	under := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+string(os.PathSeparator)) }
	if abs, err := filepath.Abs(path); err == nil && under(abs, userRoot) {
		return true
	}
	real, err := filepath.EvalSymlinks(userRoot)
	return err == nil && under(resolved, real)
}

// userRootAncestryProblems checks the directories another user could use to
// swap the per-user root: every directory a name is looked up in while
// $HOME/.local/lib/capsulectl/plugins resolves from $HOME, including those
// inside each symlink's target (so the holder of every hop of a symlink
// chain is seen), the resolved root itself, and the parents of each of
// these up to $HOME, or up to / for one outside $HOME. Each must pass
// checkTrustedComponent, with a root-owned sticky directory (such as /tmp)
// accepted, as install's full-path check accepts it. Every symlink followed
// must be owned by the user or root: in a sticky directory, which only
// stops others from replacing entries they do not own, a link another user
// owns is still theirs to repoint. No group is exempt: a
// group's name or gid proves nothing about who else is in it, so a stock
// Ubuntu (umask 002) ~/.local is refused until chmod g-w. A missing
// component ends the lookup (with no root there is nothing to run); the
// directories looked up in so far are still checked.
func userRootAncestryProblems(home string) []unsafePathProblem {
	if !filepath.IsAbs(home) {
		return []unsafePathProblem{{path: home, unresolved: true, err: fmt.Errorf("HOME=%s is not an absolute path", home)}}
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return []unsafePathProblem{{path: home, unresolved: true, err: fmt.Errorf("cannot resolve %s: %w", home, err)}}
	}
	var problems []unsafePathProblem
	hops := 0
	var links []string
	root, dirs, err := lookupDirs(realHome, filepath.Join(".local", "lib", "capsulectl", "plugins"), &hops, &links)
	if err == nil {
		dirs = append(dirs, root)
	} else if !errors.Is(err, fs.ErrNotExist) {
		problems = append(problems, unsafePathProblem{path: userPluginRoot(home), unresolved: true, err: fmt.Errorf("cannot resolve %s: %w", userPluginRoot(home), err)})
	}
	uid := os.Getuid()
	problems = append(problems, linkOwnerProblems(links, uid)...)
	seen := map[string]bool{}
	for _, dir := range dirs {
		for p := dir; !seen[p]; {
			seen[p] = true
			if err := checkTrustedComponent(p, uid, true); err != nil {
				problems = append(problems, unsafePathProblem{path: p, owner: errors.Is(err, errNotTrustedOwner), err: err})
			}
			parent := filepath.Dir(p)
			if p == realHome || parent == p {
				break
			}
			p = parent
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].path < problems[j].path })
	return problems
}

// linkOwnerProblems is every link owned by neither uid nor root.
func linkOwnerProblems(links []string, uid int) []unsafePathProblem {
	var problems []unsafePathProblem
	for _, link := range links {
		info, err := os.Lstat(link)
		if err != nil {
			problems = append(problems, unsafePathProblem{path: link, unresolved: true, err: err})
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			problems = append(problems, unsafePathProblem{path: link, owner: true, err: fmt.Errorf("cannot read ownership of %s", link)})
		} else if int(stat.Uid) != uid && stat.Uid != 0 {
			problems = append(problems, unsafePathProblem{path: link, owner: true, err: fmt.Errorf("%s is a symlink %w", link, errNotTrustedOwner)})
		}
	}
	return problems
}

// lookupDirs resolves the relative path rel from dir, a resolved directory,
// one name at a time as the kernel does, following each symlink through its
// target. It returns the resolved path and every directory a name was
// looked up in, in order, and appends each symlink followed to links. hops
// bounds the symlinks followed.
func lookupDirs(dir, rel string, hops *int, links *[]string) (string, []string, error) {
	cur := dir
	var dirs []string
	for _, name := range strings.Split(rel, string(os.PathSeparator)) {
		switch name {
		case "", ".":
			continue
		case "..":
			dirs = append(dirs, cur)
			cur = filepath.Dir(cur)
			continue
		}
		dirs = append(dirs, cur)
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", dirs, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		*links = append(*links, next)
		if *hops++; *hops > 40 {
			return "", dirs, fmt.Errorf("%s: too many levels of symbolic links", next)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", dirs, err
		}
		base := cur
		if filepath.IsAbs(target) {
			base = string(os.PathSeparator)
		}
		resolved, sub, err := lookupDirs(base, target, hops, links)
		dirs = append(dirs, sub...)
		if err != nil {
			return "", dirs, err
		}
		cur = resolved
	}
	return cur, dirs, nil
}

// ancestryError is the walk's refusal for an unsafe per-user root ancestry:
// install's message and fix, unwrapping to the sentinel the presentation
// loader classifies by.
type ancestryError struct {
	message  string
	sentinel error
}

func (e *ancestryError) Error() string { return e.message }
func (e *ancestryError) Unwrap() error { return e.sentinel }

func newAncestryError(what string, problems []unsafePathProblem) error {
	// A refusal only for paths that do not resolve is a resolution
	// failure, not a permission one.
	sentinel := errOutsideRoots
	for _, p := range problems {
		switch {
		case p.owner:
			sentinel = errNotTrustedOwner
		case !p.unresolved && sentinel != errNotTrustedOwner:
			sentinel = errWritable
		}
	}
	return &ancestryError{message: unsafePathMessage(what, problems), sentinel: sentinel}
}

// checkTrustedComponent is the trust rule for one path component: owned by
// uid or root, and neither group- nor other-writable. allowSticky also
// accepts a root-owned directory with the sticky bit set (such as /tmp),
// where other users cannot rename or remove entries they do not own. Only
// install's full-path check and the per-user root's ancestry check pass it;
// the walk from a launcher up to its root keeps the strict rule.
func checkTrustedComponent(p string, uid int, allowSticky bool) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read ownership of %s", p)
	}
	if int(stat.Uid) != uid && stat.Uid != 0 {
		return fmt.Errorf("%s is %w", p, errNotTrustedOwner)
	}
	mode := info.Mode()
	if allowSticky && stat.Uid == 0 && mode.IsDir() && mode&os.ModeSticky != 0 {
		return nil
	}
	if mode&0o020 != 0 {
		return fmt.Errorf("%s is group-writable: %w", p, errWritable)
	} else if mode&0o002 != 0 {
		return fmt.Errorf("%s is other-writable: %w", p, errWritable)
	}
	return nil
}

type pluginInfo struct {
	Name        string   `json:"name"`
	Vendor      string   `json:"vendor"`
	Version     string   `json:"version"`
	PluginAPI   string   `json:"plugin_api"`
	Subcommands []string `json:"subcommands,omitempty"`
	// Presentations are the modules the plugin offers a page builder
	// (plugin_presentations.go). An older capsulectl ignores the member.
	Presentations []pluginPresentation `json:"presentations,omitempty"`
	path          string
	// checked is the path the handshake executed. For a discovered plugin
	// that is the resolved path the trust walk judged, and presentations
	// load beside it; install's handshake on a file it staged itself sets it
	// to that file.
	checked string
	// presentations are the checked modules, or refusal why there are none.
	presentations []presentationModule
	refusal       *presentationRefusal
}

// pluginMetadata runs the launcher's cli-plugin-metadata handshake. A launcher
// that does not answer, advertises a different plugin_api, or names a command
// other than its own filename's (capsulectl-<name> must say "name": "<name>")
// is refused (returned as an error) rather than trusted. The name check is what
// lets discovery dedupe on the filename while dispatch uses the metadata name:
// for every accepted launcher the two are the same string.
func pluginMetadata(path, name string) (pluginInfo, error) {
	return pluginMetadataAs(path, path, name)
}

// pluginMetadataAs is pluginMetadata running execPath, the path the trust
// walk checked, with argv[0] and the plugin's recorded path set to argv0,
// the name discovery found it by.
func pluginMetadataAs(execPath, argv0, name string) (pluginInfo, error) {
	info, err := readPluginMetadataAs(execPath, argv0, name)
	if err != nil {
		return pluginInfo{}, err
	}
	if info.Name != name {
		return pluginInfo{}, fmt.Errorf("%s: metadata names the command %q, but the launcher's filename names %q; they must match", name, info.Name, name)
	}
	return info, nil
}

// readPluginMetadata runs the handshake and checks plugin_api, but not the
// name: install reads the name from a launcher whose filename carries none.
// label names the launcher in errors.
func readPluginMetadata(path, label string) (pluginInfo, error) {
	return readPluginMetadataAs(path, path, label)
}

// readPluginMetadataAs is readPluginMetadata running execPath with argv[0]
// set to argv0, which is also the path recorded on the result.
func readPluginMetadataAs(execPath, argv0, label string) (pluginInfo, error) {
	name := label
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := launcherCommand(ctx, execPath, argv0, "cli-plugin-metadata")
	// WaitDelay bounds the wait even if a forked descendant keeps the stdout pipe
	// open after the timeout kills the direct process, so a misbehaving launcher
	// cannot hang discovery indefinitely.
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return pluginInfo{}, fmt.Errorf("%s: cli-plugin-metadata handshake failed: %w", name, err)
	}
	var info pluginInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return pluginInfo{}, fmt.Errorf("%s: cli-plugin-metadata is not JSON: %w", name, err)
	}
	if info.PluginAPI != PluginAPI {
		return pluginInfo{}, fmt.Errorf("%s: plugin_api %q does not match %q", name, info.PluginAPI, PluginAPI)
	}
	info.path, info.checked = argv0, execPath
	return info, nil
}

// launcherCommand runs checked, the path the trust walk judged, with argv[0]
// set to name, the path the launcher was found by, so a launcher that reads
// its own argv[0] sees what it did before.
func launcherCommand(ctx context.Context, checked, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, checked, args...)
	cmd.Args[0] = name
	return cmd
}

// refusedPlugin is a capsulectl-<name> launcher discovery found on a trusted root
// but did not wire up, with the reason. `plugin ls` reports these so an operator
// learns why a launcher is missing instead of seeing it silently absent.
type refusedPlugin struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// discoverPlugins scans the trusted roots for capsulectl-<name> launchers, in root
// order (an earlier root shadows a later one for the same name), keeping only those
// that pass the trusted-path check and a valid metadata handshake.
func discoverPlugins() []pluginInfo {
	plugins, _ := discoverPluginsAndRefusals()
	return plugins
}

// discoverPluginsAndRefusals is discoverPlugins plus every refused launcher and
// its reason. Deduplication is by filename: the first accepted
// capsulectl-<name> wins, and a refused one does not shadow a later candidate.
func discoverPluginsAndRefusals() ([]pluginInfo, []refusedPlugin) {
	var plugins []pluginInfo
	var refused []refusedPlugin
	seen := map[string]bool{}
	for _, root := range trustedPluginRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			base := entry.Name()
			if !strings.HasPrefix(base, pluginLauncherPrefix) {
				continue
			}
			name := strings.TrimPrefix(base, pluginLauncherPrefix)
			if name == "" || seen[name] {
				continue
			}
			path := filepath.Join(root, base)
			checked, err := CheckedTrustedPath(path)
			if err != nil {
				refused = append(refused, refusedPlugin{Path: path, Reason: err.Error()})
				continue
			}
			info, err := pluginMetadataAs(checked, path, name)
			if err != nil {
				refused = append(refused, refusedPlugin{Path: path, Reason: err.Error()})
				continue
			}
			// A plugin whose presentations are refused still dispatches.
			if info.presentations, err = loadPresentations(info); err != nil {
				info.presentations, info.refusal = nil, asRefusal(err)
			}
			seen[name] = true
			plugins = append(plugins, info)
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	return plugins, refused
}

// pluginBeforeExec runs between dispatch's trust check and its exec (tests).
var pluginBeforeExec = func() {}

// execPlugin re-verifies the launcher's trusted path at dispatch time (closing
// the discovery->dispatch window), then execs the resolved path the check
// judged, never the name it was found by, with the given args passed straight
// through. A symlink on that name swapped after the check reaches nothing
// that runs; what remains is the resolved path's own chain, which the walk
// holds to the user or root (closing that would need an O_PATH fd).
func execPlugin(c *cobra.Command, info pluginInfo, args []string) error {
	checked, err := CheckedTrustedPath(info.path)
	if err != nil {
		return InputError("refusing to run plugin from an untrusted path")
	}
	pluginBeforeExec()
	if warning := driftWarning(info); warning != "" {
		fmt.Fprintln(c.ErrOrStderr(), warning)
	}
	cmd := launcherCommand(c.Context(), checked, info.path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.InOrStdin(), stdoutFile(c.OutOrStdout()), c.ErrOrStderr()
	// The profile a plugin inherits is only the one this run selected
	// (profileInArgs): a CAPSULECTL_PROFILE exported in the operator's shell
	// never reaches it, or the plugin would read it as selected.
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, ProfileEnv+"=")
	}), "CAPSULECTL_PLUGIN_API="+PluginAPI)
	// The profile this command line selects goes with it, so a command the
	// plugin runs back in the core uses the same one. --profile anywhere in
	// args still wins inside the plugin.
	if name := profileInArgs(args); name != "" {
		cmd.Env = append(cmd.Env, ProfileEnv+"="+name)
	}
	// A plugin that calls core back (sealing, verify, profile show) needs to
	// find this binary; on a host whose PATH lacks the install folder (a Muse
	// chat never has ~/.local/bin on it) a bare "capsulectl" fails. Core knows
	// its own path, so it is handed over, unless the caller already set it.
	if os.Getenv("CAPSULECTL_BIN") == "" {
		if self, err := selfExecutable(); err == nil {
			cmd.Env = append(cmd.Env, "CAPSULECTL_BIN="+self)
		}
	}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() < 0 {
				// Killed by a signal: the plugin said nothing, and -1 would
				// reach the shell as 255, a code the plugin could have chosen.
				return Hint(errPluginStopped, fmt.Sprintf("%s stopped before it finished (%s); check the store before retrying", info.Name, exit.String()))
			}
			return &PluginExit{Code: exit.ExitCode()}
		}
		// Not started at all (removed, or no longer executable, since
		// discovery): the operator needs the plugin's name, not the generic
		// failure line.
		return errors.Join(Hint(errPluginStopped, fmt.Sprintf("plugin %s could not be started; check it with `capsulectl plugin ls`", info.Name)), errPluginNotStarted, err)
	}
	return nil
}

// errPluginStopped is the class of a plugin that did not run to its own
// exit: killed by a signal, or never started.
var errPluginStopped = errors.New("plugin stopped")

// PluginExit is a plugin's own non-zero exit. The plugin has already said
// why on stderr, so capsulectl adds nothing to it (SafeError is empty) and
// exits with the plugin's code (ExitCode). It is the error class of that
// code, so a caller can still tell an input error from a pending one.
type PluginExit struct{ Code int }

func (e *PluginExit) Error() string { return fmt.Sprintf("the plugin exited with status %d", e.Code) }

func (e *PluginExit) Unwrap() error {
	switch e.Code {
	case 2:
		return ErrInput
	case 3:
		return ErrPartial
	case 4:
		return ErrPending
	case 5:
		return ErrConflict
	case 6:
		return ErrAlarm
	case 7:
		return ErrPaused
	}
	return nil
}

// selfExecutable is the running capsulectl's path, symlinks resolved when
// they can be, for CAPSULECTL_BIN.
func selfExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		return resolved, nil
	}
	return self, nil
}

// pluginCommand wraps a discovered launcher as a cobra command that execs it,
// passing the remaining args through unchanged (flag parsing disabled so the
// plugin owns its own flags). A plugin never shadows a core command: cobra does
// NOT ignore a duplicate Use, so addPluginCommands explicitly skips any name
// already reserved by a core command (and by help/completion).
func pluginCommand(info pluginInfo) *cobra.Command {
	return &cobra.Command{
		Use:                info.Name,
		Short:              fmt.Sprintf("%s (plugin · %s)", FirstNonEmpty(pluginShort(info), "external plugin"), FirstNonEmpty(info.Vendor, "unknown vendor")),
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			return execPlugin(c, info, args)
		},
	}
}

func pluginShort(info pluginInfo) string {
	if len(info.Subcommands) == 0 {
		return ""
	}
	return "subcommands: " + strings.Join(info.Subcommands, ", ")
}

func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// pluginGroup is the `plugin` command group: `plugin ls` lists the
// discovered launchers, and `plugin install` and `plugin remove` manage the
// per-user root (plugin_install.go). There is no enable/disable state: a
// launcher on a trusted root is the whole of a plugin's installation.
func pluginGroup() *cobra.Command {
	group := &cobra.Command{Use: "plugin", Short: "List, install and remove capsulectl-* plugins"}
	ls := &cobra.Command{Use: "ls", Short: "List trusted, handshake-valid plugins discovered on the plugin roots", Args: NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			plugins, refused := discoverPluginsAndRefusals()
			rows := make([]map[string]any, 0, len(plugins))
			for _, p := range plugins {
				row := map[string]any{"name": p.Name, "vendor": p.Vendor, "version": p.Version,
					"plugin_api": p.PluginAPI, "subcommands": p.Subcommands, "path": p.path}
				if len(p.Presentations) > 0 {
					modules := []map[string]any{}
					for _, m := range p.presentations {
						modules = append(modules, map[string]any{"id": m.ID, "trust_class": m.TrustClass,
							"presentation_api": m.Manifest["presentation_api"], "runtime_min": m.Manifest["runtime_min"]})
					}
					row["presentations"] = modules
					if p.refusal != nil {
						row["presentations_refused"] = p.refusal
					}
				}
				rows = append(rows, row)
			}
			if refused == nil {
				refused = []refusedPlugin{}
			}
			out := map[string]any{"roots": trustedPluginRoots(), "plugins": rows, "refused": refused}
			if left := leftoverInstalls(); len(left) > 0 {
				out["interrupted_installs"] = left
			}
			return Output(c, out)
		}}
	group.AddCommand(ls, pluginInstallCommand(), pluginRemoveCommand())
	return group
}

// addPluginCommands registers the plugin group and every discovered plugin as a
// top-level command. A plugin can never shadow a core command or cobra's built-in
// help/completion: the reserved set seeds those explicitly (help/completion are
// added by cobra at execute time, not visible in root.Commands() here), and a
// launcher whose name is not a single token is skipped, since cobra would derive
// its command name from the first whitespace-delimited token and could otherwise
// alias a core command.
func addPluginCommands(root *cobra.Command) {
	reserved := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		reserved[c.Name()] = true
	}
	plugins, refused := discoverPluginsAndRefusals()
	for _, info := range plugins {
		if strings.ContainsAny(info.Name, " \t") {
			continue
		}
		cmd := pluginCommand(info)
		if reserved[cmd.Name()] {
			continue
		}
		root.AddCommand(cmd)
		reserved[cmd.Name()] = true
	}
	// A refused launcher still answers its name, with why and the fix, when
	// the name is one an accepted plugin could have: whoever could write the
	// refused one chose its filename, which must not reach help or errors
	// unchecked. Any other stays in `plugin ls` only. A first-party one
	// answers through its placeholder below.
	for _, r := range refused {
		name := strings.TrimPrefix(filepath.Base(r.Path), pluginLauncherPrefix)
		if !pluginNameGrammar.MatchString(name) || reserved[name] || name == BookPluginName || name == DealPluginName {
			continue
		}
		root.AddCommand(refusedPluginPlaceholder(name, refused))
		reserved[name] = true
	}
	// A first-party plugin that is not installed still answers its name, to
	// say what to install.
	for name, short := range map[string]string{BookPluginName: "Evidence books: close, reconcile, request, respond, remote, result, report, backfill, migrate", DealPluginName: "Deals: open, check, close, report, request, respond, compose, ..."} {
		if !reserved[name] {
			root.AddCommand(firstPartyPlaceholder(name, short, refused))
		}
	}
}
