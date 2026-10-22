package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// A host may loosen the per-user plugin root's ancestry after install: a Muse
// VM replacement recreates /home/hatch as 2770 (capsule-cli#336). The walk
// then refuses every plugin there and the scheduled checkpoint cadence stops.
// healUserRoot puts back the chmod go-w that plugin install already required
// of the same chain, under conditions narrow enough that it can only tighten
// directories the process owns under $HOME. It never changes the walk:
// dispatch judges the result as before.

// userRootHeal is one heal attempt, reported by doctor and written to the
// cadence log.
type userRootHeal struct {
	// State is ok (nothing to heal), absent (no per-user plugin root, so
	// nothing is gated), healed, or refused (not healable; Error says why).
	State         string               `json:"state"`
	ChmodGoW      []string             `json:"chmod_go_w,omitempty"`
	Modes         map[string][2]string `json:"modes,omitempty"`
	SetgidDropped []string             `json:"setgid_dropped,omitempty"`
	Failed        map[string]string    `json:"failed,omitempty"`
	Reason        string               `json:"reason,omitempty"`
	Error         string               `json:"error,omitempty"`
	// why is a refusal's reason alone, for install to add to its own.
	why string
}

const userRootHealedReason = "these directories on the plugin root's path were group- or other-writable and owned by this user; plugin install required them not to be, and a host may reset them (for example on VM replacement), so write access for group and others was removed"

// Seams for tests, which cannot run as root or chown.
var (
	// healProcess is the process uid, gid and supplementary groups.
	healProcess = func() (uid, gid int, groups []int) {
		groups, _ = os.Getgroups()
		return os.Getuid(), os.Getgid(), groups
	}
	// healOwner is the owner the heal judges an opened directory by.
	healOwner = func(st *unix.Stat_t) int { return int(st.Uid) }
	// healBeforePreflight runs between the ancestry check and preflight.
	healBeforePreflight = func() {}
	// healChmod changes an opened directory's mode.
	healChmod = func(fd int, mode uint32) error { return unix.Fchmod(fd, mode) }
)

// healUserRoot checks the per-user plugin root's ancestry and, when every
// refused component is a directory the process owns at or under $HOME whose
// only fault is group or other write, removes those bits. It changes nothing
// otherwise.
func healUserRoot(home string) userRootHeal {
	if !userRootIsTrusted(home) {
		return userRootHeal{State: "absent", Reason: userPluginRoot(home) + " is not one of the plugin roots"}
	}
	if _, err := filepath.EvalSymlinks(userPluginRoot(home)); err != nil {
		return userRootHeal{State: "absent", Reason: userPluginRoot(home) + " does not resolve"}
	}
	problems := userRootAncestryProblems(home)
	if len(problems) == 0 {
		return userRootHeal{State: "ok"}
	}
	return healProblems(home, problems, userRootHealedReason)
}

// healProblems is healUserRoot's repair of problems found on the plugin
// root's path; reason is reported when it heals.
func healProblems(home string, problems []unsafePathProblem, reason string) userRootHeal {
	refuse := func(why string) userRootHeal {
		return userRootHeal{State: "refused", Error: newAncestryError("the plugin root "+userPluginRoot(home), problems).Error() + "; not healed: " + why, why: why}
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil || realHome == string(os.PathSeparator) {
		return refuse("HOME does not resolve to a directory below /")
	}
	uid, gid, groups := healProcess()
	if uid == 0 && !homeDeepEnoughForRoot(realHome) {
		return refuse("as root, HOME must be at least two levels below /")
	}
	var rels []string
	for _, p := range problems {
		if p.owner || p.unresolved || !errors.Is(p.err, errWritable) {
			return refuse(p.path + " is not only group- or other-writable")
		}
		rel, err := filepath.Rel(realHome, p.path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return refuse(p.path + " is outside HOME")
		}
		rels = append(rels, rel)
	}

	healBeforePreflight()
	// Preflight: open every directory without following a link at any level
	// (a plain open or Lstat follows a link a group member swaps into $HOME
	// after the check), and judge the object actually opened.
	type target struct {
		path       string
		fd         int
		mode, want uint32
	}
	var targets []target
	defer func() {
		for _, t := range targets {
			unix.Close(t.fd)
		}
	}()
	for i, rel := range rels {
		path := problems[i].path
		fd, err := openNoFollow(realHome, rel)
		if err != nil {
			return refuse(fmt.Sprintf("%s: %v", path, err))
		}
		targets = append(targets, target{path: path, fd: fd})
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return refuse(fmt.Sprintf("%s: %v", path, err))
		}
		mode := uint32(st.Mode) & 0o7777
		switch {
		case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
			return refuse(path + " is not a directory")
		case healOwner(&st) != uid:
			return refuse(path + " is not owned by this process's user")
		case mode&unix.S_ISVTX != 0:
			return refuse(path + " is sticky (shared on purpose)")
		case mode&unix.S_ISGID != 0 && uid != 0 && int(st.Gid) != gid && !slices.Contains(groups, int(st.Gid)):
			return refuse(path + " is setgid to a group this process is not in, so chmod would drop setgid")
		}
		targets[len(targets)-1].mode, targets[len(targets)-1].want = mode, mode&^0o022
	}

	// Apply. A directory already tightened stays so if a later one fails:
	// undoing it would add write access back.
	h := userRootHeal{State: "healed", Reason: reason, Modes: map[string][2]string{}}
	for _, t := range targets {
		if t.mode == t.want {
			continue // a concurrent run tightened it first
		}
		if err := healChmod(t.fd, t.want); err != nil {
			h.fail(t.path, err.Error())
			continue
		}
		// The chmod happened: it is reported whatever the check after says.
		h.ChmodGoW = append(h.ChmodGoW, t.path)
		var st unix.Stat_t
		if err := unix.Fstat(t.fd, &st); err != nil {
			h.Modes[t.path] = [2]string{fmt.Sprintf("%o", t.mode), "unverified"}
			h.fail(t.path, "chmod done, mode not verified: "+err.Error())
			continue
		}
		got := uint32(st.Mode) & 0o7777
		h.Modes[t.path] = [2]string{fmt.Sprintf("%o", t.mode), fmt.Sprintf("%o", got)}
		switch got {
		case t.want:
		case t.want &^ unix.S_ISGID:
			h.SetgidDropped = append(h.SetgidDropped, t.path)
		default:
			h.fail(t.path, fmt.Sprintf("mode is %o after chmod, want %o", got, t.want))
		}
	}
	if len(h.ChmodGoW) == 0 {
		if len(h.Failed) == 0 {
			return userRootHeal{State: "ok"} // a concurrent run healed it all
		}
		h.State, h.Reason, h.Modes = "refused", "", nil
		h.Error = "chmod go-w failed on every directory it was to tighten"
	}
	return h
}

func (h *userRootHeal) fail(path, why string) {
	if h.Failed == nil {
		h.Failed = map[string]string{}
	}
	h.Failed[path] = why
}

// homeDeepEnoughForRoot: as root, the heal acts only on a HOME at least two
// levels below / (/home/hatch, not /tmp or /srv), so a root process with an
// odd HOME never tightens a tree shared on purpose.
func homeDeepEnoughForRoot(realHome string) bool {
	return len(strings.Split(strings.Trim(realHome, string(os.PathSeparator)), string(os.PathSeparator))) >= 2
}

const installPathHealedReason = "these directories on the plugin root's path were group- or other-writable and owned by this user; plugin install requires them not to be, so write access for group and others was removed"

// healInstallPath is healUserRoot for plugin install, run before its own
// checks: a host that makes HOME group-writable would otherwise stop every
// fresh install (capsule-cli#354). The root may not exist yet, so it judges
// the existing part of the path from / down as install does, plus the
// directories the walk looks names up in so far. A problem above HOME
// refuses the whole heal, and installRoot then refuses as before.
func healInstallPath(home string) userRootHeal {
	if !userRootIsTrusted(home) {
		return userRootHeal{State: "absent", Reason: userPluginRoot(home) + " is not one of the plugin roots"}
	}
	_, existing, _, err := canonicalPath(userPluginRoot(home))
	if err != nil {
		return userRootHeal{State: "absent", Reason: userPluginRoot(home) + " does not resolve"}
	}
	problems := fullPathCheck(existing)
	for _, p := range userRootAncestryProblems(home) {
		if !slices.ContainsFunc(problems, func(q unsafePathProblem) bool { return q.path == p.path }) {
			problems = append(problems, p)
		}
	}
	if len(problems) == 0 {
		return userRootHeal{State: "ok"}
	}
	return healProblems(home, problems, installPathHealedReason)
}

// explain adds to install's error what its heal did: why it healed nothing,
// each chmod that failed, and which directories it tightened, since they
// stay so. A message SafeError hides goes to stderr instead.
func (h userRootHeal) explain(c *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	var notes []string
	if h.why != "" {
		notes = append(notes, "not healed: "+h.why)
	}
	for _, path := range slices.Sorted(maps.Keys(h.Failed)) {
		notes = append(notes, "chmod go-w on "+path+": "+h.Failed[path])
	}
	if len(h.ChmodGoW) > 0 {
		notes = append(notes, "install had already removed group and other write from "+strings.Join(h.ChmodGoW, " "))
	}
	if len(notes) == 0 {
		return err
	}
	var hint *hintError
	if errors.As(err, &hint) {
		return Hint(hint.class, hint.message+"; "+strings.Join(notes, "; "))
	}
	fmt.Fprintln(c.ErrOrStderr(), "capsulectl: "+strings.Join(notes, "; "))
	return err
}

// doctorUserRoot is healUserRoot for doctor, which the user ran.
func doctorUserRoot() userRootHeal {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return userRootHeal{State: "absent", Reason: "HOME is not set"}
	}
	h := healUserRoot(home)
	// ok also covers a concurrent run that healed the chain first.
	if h.State == "healed" || h.State == "ok" {
		// Doctor reports what the walk now says, not only what it changed.
		if problems := userRootAncestryProblems(home); len(problems) > 0 {
			h.State = "refused"
			h.Error = newAncestryError("the plugin root "+userPluginRoot(home), problems).Error()
		}
	}
	return h
}

// openNoFollow opens dir/rel as a directory, following no symlink in dir's
// last component or in any component of rel.
func openNoFollow(dir, rel string) (int, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open(dir, flags, 0)
	if err != nil {
		return -1, err
	}
	for _, name := range strings.Split(rel, string(os.PathSeparator)) {
		if name == "" || name == "." {
			continue
		}
		next, err := unix.Openat(fd, name, flags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

// userRootIsTrusted reports whether the per-user root is one of the plugin
// roots (CAPSULECTL_PLUGIN_ROOTS may leave it out), compared canonically as
// installRoot compares it.
func userRootIsTrusted(home string) bool {
	user, _, _, err := canonicalPath(userPluginRoot(home))
	if err != nil {
		return false
	}
	for _, r := range trustedPluginRoots() {
		if c, _, _, err := canonicalPath(r); err == nil && c == user {
			return true
		}
	}
	return false
}

// forwardCadence is `cll checkpoint cadence` in a core without the deal
// commands: `tick` in capsulectl-deal. Its output is the scheduled run's log,
// so it heals a per-user root a host loosened, and says on stdout what it
// healed and why the plugin did not run; stderr and the exit code are as
// ForwardToPlugin leaves them.
func forwardCadence(c *cobra.Command, args []string) error {
	plugins, refused := discoverPluginsAndRefusals()
	if home, err := os.UserHomeDir(); err == nil && home != "" && userDealRefused(home, plugins, refused) {
		h := healUserRoot(home)
		if len(h.ChmodGoW)+len(h.Failed) > 0 {
			// The cadence log's states name what they are about.
			line := h
			line.State = "plugin_root_healed"
			if len(h.ChmodGoW) == 0 {
				line.State = "plugin_root_heal_failed"
			}
			if err := Output(c, line); err != nil {
				return err
			}
		}
		// Discover again unless nothing could have changed: a concurrent
		// run may have healed the chain since (healUserRoot says ok).
		if h.State != "absent" && !(h.State == "refused" && len(h.ChmodGoW) == 0) {
			plugins, refused = discoverPluginsAndRefusals()
		}
	}
	err := forwardDiscovered(c, DealPluginName, append([]string{"tick"}, args...), plugins, refused)
	if err == nil {
		return nil
	}
	state := "plugin_unavailable"
	var exit *PluginExit
	switch {
	case errors.As(err, &exit):
		return err // the plugin ran, and said why itself
	case errors.Is(err, errPluginStopped):
		state = "plugin_stopped" // killed after it started, or never started
	}
	if outErr := Output(c, map[string]any{"state": state, "error": SafeError(err)}); outErr != nil {
		return errors.Join(err, outErr)
	}
	return err
}

// userDealRefused: no deal plugin was accepted, and the per-user root's deal
// launcher was refused. A valid deal plugin in another root needs no heal.
func userDealRefused(home string, plugins []pluginInfo, refused []refusedPlugin) bool {
	for _, p := range plugins {
		if p.Name == DealPluginName {
			return false
		}
	}
	userRoot, _, _, err := canonicalPath(userPluginRoot(home))
	if err != nil {
		return false
	}
	for _, r := range refused {
		if filepath.Base(r.Path) != pluginLauncherPrefix+DealPluginName {
			continue
		}
		if dir, _, _, err := canonicalPath(filepath.Dir(r.Path)); err == nil && dir == userRoot {
			return true
		}
	}
	return false
}
