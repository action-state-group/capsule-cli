package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// `plugin install` and `plugin remove` place first-party and third-party
// launchers on the per-user plugin root (docs/PLUGINS.md, "Installing
// plugins"). Dispatch keeps the trust walk unchanged; install is stricter
// until capsule-cli#270 hardens the walk: before it writes or executes
// anything it resolves the path once, checks every component of the
// resolved path from / down, and from then on uses only that canonical path.

// releaseTagPolicy is the shape of a release tag, enforced in release.yml
// too: a core whose CLIVersion matches it is a release build.
var releaseTagPolicy = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+(\.[0-9A-Za-z]+)*)?$`)

func isReleaseCore() bool { return releaseTagPolicy.MatchString(CLIVersion) }

// pluginNameGrammar is the name a launcher may install under: one basename
// token, so capsulectl-<name> is always a direct child of the root.
var pluginNameGrammar = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func isFirstParty(name string) bool { return name == BookPluginName || name == DealPluginName }

// releasePlatforms are the targets scripts/release-build.sh ships.
var releasePlatforms = []string{"linux/amd64", "linux/arm64", "darwin/arm64"}

const (
	pluginReleaseBase   = "https://github.com/action-state-group/capsule-cli/releases/download"
	pluginInstallPrefix = ".install-"
	pluginReadIdle      = 60 * time.Second
	ghVerifyTimeout     = 2 * time.Minute
)

// Download caps, per kind; variables so tests can lower them.
var (
	maxPluginAsset  int64 = 128 << 20
	maxPluginSums   int64 = 64 << 10
	maxPluginBundle int64 = 4 << 20
)

var (
	errPluginDurability = errors.New("durability not confirmed")
	// errPluginFetch is a download, or a provenance check that could not
	// run: nothing was installed, and retrying may succeed (exit 1).
	errPluginFetch = errors.New("plugin not installed")
)

// pluginDownloadClient fetches release assets. Every hop must stay on HTTPS
// (GitHub redirects assets to its object host).
var pluginDownloadClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s://%s: downloads stay on HTTPS", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	},
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 60 * time.Second},
}

// requireOwnInstall refuses a run on another user's behalf: through sudo,
// or by a process that does not own $HOME. Root installing into its own
// home (a container) is allowed. It returns $HOME.
func requireOwnInstall() (string, error) {
	if os.Getenv("SUDO_UID") != "" {
		return "", InputError("plugin install and remove run as the user whose plugins they manage, never through sudo (SUDO_UID is set): run it without sudo")
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", InputError("plugin install and remove need HOME, the directory whose .local/lib/capsulectl/plugins they manage")
	}
	info, err := os.Stat(home)
	if err != nil {
		return "", InputError("HOME=" + home + " cannot be read")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot read the owner of %s", home)
	}
	if int(stat.Uid) != os.Geteuid() {
		return "", InputError(fmt.Sprintf("HOME=%s is owned by uid %d, but this process runs as uid %d: plugin install and remove run as the owner of HOME (set HOME to your own home directory)", home, stat.Uid, os.Geteuid()))
	}
	return home, nil
}

func userPluginRoot(home string) string {
	return filepath.Join(home, ".local", "lib", "capsulectl", "plugins")
}

// canonicalPath resolves the longest existing prefix of path with
// EvalSymlinks and appends the missing components unchanged. It returns the
// canonical path, the existing canonical prefix, and the missing names from
// the top down. It creates nothing.
func canonicalPath(path string) (canonical, existing string, missing []string, err error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", "", nil, err
	}
	var rev []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			existing = real
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", "", nil, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", "", nil, err
		}
		rev = append(rev, filepath.Base(p))
		p = parent
	}
	for i := len(rev) - 1; i >= 0; i-- {
		missing = append(missing, rev[i])
	}
	return filepath.Join(append([]string{existing}, missing...)...), existing, missing, nil
}

// unsafePathProblem is one component the full-path check refused.
type unsafePathProblem struct {
	path  string
	owner bool // owned by another user, else writable by others
	err   error
}

// fullPathCheck checks every component of an existing canonical path, from
// it up to /: each owned by the user or root, and neither group- nor
// other-writable, except a root-owned sticky directory. It returns every
// failing component.
func fullPathCheck(canonical string) []unsafePathProblem {
	var problems []unsafePathProblem
	uid := os.Getuid()
	for p := canonical; ; {
		if err := checkTrustedComponent(p, uid, true); err != nil {
			problems = append(problems, unsafePathProblem{path: p, owner: errors.Is(err, errNotTrustedOwner), err: err})
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	slices.Reverse(problems)
	return problems
}

// unsafePathError is one refusal listing every failing component and one
// combined fix.
func unsafePathError(what string, problems []unsafePathProblem) error {
	var writable, owned, reasons []string
	for _, p := range problems {
		reasons = append(reasons, p.err.Error())
		if p.owner {
			owned = append(owned, p.path)
		} else {
			writable = append(writable, p.path)
		}
	}
	var fixes []string
	if len(writable) > 0 {
		fixes = append(fixes, "chmod go-w "+strings.Join(writable, " "))
	}
	if len(owned) > 0 {
		fixes = append(fixes, "make "+strings.Join(owned, " ")+" yours (chown) or move the plugin root off it")
	}
	return InputError(fmt.Sprintf("%s is on a path another user could change (%s); fix: %s", what, strings.Join(reasons, "; "), strings.Join(fixes, "; then ")))
}

// installRoot returns the canonical per-user plugin root, creating missing
// components one Mkdir at a time under verified parents, after the full-path
// check. With CAPSULECTL_PLUGIN_ROOTS set, the list must contain it.
func installRoot(home string, create bool) (string, error) {
	target := userPluginRoot(home)
	canonical, existing, missing, err := canonicalPath(target)
	if err != nil {
		return "", err
	}
	if os.Getenv("CAPSULECTL_PLUGIN_ROOTS") != "" {
		listed := false
		for _, r := range trustedPluginRoots() {
			if c, _, _, err := canonicalPath(r); err == nil && c == canonical {
				listed = true
			}
		}
		if !listed {
			return "", InputError("CAPSULECTL_PLUGIN_ROOTS does not include " + target + ", the only root plugin install and remove manage; add it to the list or unset the variable")
		}
	}
	if problems := fullPathCheck(existing); len(problems) > 0 {
		return "", unsafePathError("the plugin root "+target, problems)
	}
	if len(missing) > 0 && !create {
		return "", InputError("no plugin is installed: " + target + " does not exist")
	}
	parent := existing
	for _, name := range missing {
		r, err := os.OpenRoot(parent)
		if err != nil {
			return "", err
		}
		err = r.Mkdir(name, 0o755)
		if errors.Is(err, fs.ErrExist) {
			// A concurrent install created it first: accept a directory (never
			// a symlink); the full-path check below judges its trust.
			if st, lerr := r.Lstat(name); lerr == nil && st.IsDir() {
				err = nil
			}
		}
		r.Close()
		if err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Join(parent, name), err)
		}
		parent = filepath.Join(parent, name)
	}
	if problems := fullPathCheck(canonical); len(problems) > 0 {
		return "", unsafePathError("the plugin root "+target, problems)
	}
	// The root itself also meets the walk's strict rule, which dispatch runs.
	if err := checkTrustedComponent(canonical, os.Getuid(), false); err != nil {
		return "", unsafePathError("the plugin root "+target, []unsafePathProblem{{path: canonical, owner: errors.Is(err, errNotTrustedOwner), err: err}})
	}
	return canonical, nil
}

// reservedPluginNames are the names dispatch never wires: core commands and
// cobra's help and completion.
func reservedPluginNames() map[string]bool {
	reserved := map[string]bool{"help": true, "completion": true}
	for _, c := range newCommand(false, nil).Commands() {
		reserved[c.Name()] = true
	}
	return reserved
}

// candidateLauncher classifies a launcher of name in one root for install's
// shadow check and remove's report, executing it only when its full path is
// safe: "" (not a candidate: absent, or the walk refuses it), "unsafe" (a
// possible launcher that was not inspected) or "accepted" (discovery would
// wire it).
func candidateLauncher(root, name string) (state, path string) {
	path = filepath.Join(root, pluginLauncherPrefix+name)
	if _, err := os.Lstat(path); err != nil {
		return "", path
	}
	if VerifyTrustedPath(path) != nil {
		return "", path
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || len(fullPathCheck(canonical)) > 0 {
		return "unsafe", path
	}
	if _, err := pluginMetadata(canonical, name); err != nil {
		return "", path
	}
	return "accepted", path
}

// otherRoots are the configured plugin roots other than the per-user one,
// in order, with whether each precedes it.
func otherRoots(userRoot string) (before, after []string) {
	seenUser := false
	for _, r := range trustedPluginRoots() {
		c, _, _, err := canonicalPath(r)
		if err != nil {
			continue
		}
		switch {
		case c == userRoot:
			seenUser = true
		case seenUser:
			after = append(after, c)
		default:
			before = append(before, c)
		}
	}
	return before, after
}

type pluginInstallResult struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Path            string   `json:"path"`
	ReplacedVersion string   `json:"replaced_version,omitempty"`
	VersionCheck    string   `json:"version_check"`
	Checks          []string `json:"checks"`
}

// stageInRoot writes a new temporary file in the root through r and returns
// its name. The caller removes it unless it is renamed.
func stageInRoot(r *os.Root, suffix string, mode os.FileMode, write func(io.Writer) error) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := pluginInstallPrefix + hex.EncodeToString(b[:]) + suffix
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err = write(f); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Chmod(name, mode)
	}
	if err != nil {
		_ = r.Remove(name)
		return "", err
	}
	return name, nil
}

// finishInstall checks the staged launcher and renames it into place.
// expected is the first-party name a by-name install fetched, or "".
func finishInstall(r *os.Root, root, staged, expected string, allowMismatch bool, checks []string) (pluginInstallResult, error) {
	info, err := readPluginMetadata(filepath.Join(root, staged), "the candidate")
	if err != nil {
		return pluginInstallResult{}, InputError("not installed: " + err.Error())
	}
	name := info.Name
	switch {
	case !pluginNameGrammar.MatchString(name):
		return pluginInstallResult{}, InputError(fmt.Sprintf("not installed: the plugin names itself %q; a plugin name is a lowercase letter then up to 62 lowercase letters, digits or hyphens", name))
	case reservedPluginNames()[name]:
		return pluginInstallResult{}, InputError(fmt.Sprintf("not installed: %q is a core command, which a plugin can never replace", name))
	case expected != "" && name != expected:
		return pluginInstallResult{}, InputError(fmt.Sprintf("not installed: the downloaded launcher names itself %q, not %q", name, expected))
	}
	if !slices.Contains(checks, "handshake (cli-plugin-metadata)") {
		checks = append(checks, "handshake (cli-plugin-metadata)")
	}
	result := pluginInstallResult{Name: name, Version: info.Version, Path: filepath.Join(root, pluginLauncherPrefix+name), Checks: checks, VersionCheck: "not applicable (not a first-party plugin)"}
	if isFirstParty(name) {
		switch {
		case !isReleaseCore():
			result.VersionCheck = "not checked: this capsulectl is a development build (" + CLIVersion + ")"
		case info.Version == CLIVersion:
			result.VersionCheck = "matches capsulectl " + CLIVersion
		case allowMismatch:
			result.VersionCheck = "mismatch allowed: plugin " + info.Version + ", capsulectl " + CLIVersion
		default:
			return pluginInstallResult{}, Hint(ErrConflict, fmt.Sprintf("not installed: capsulectl-%s is %s but this capsulectl is %s; install the matching release, or pass --allow-version-mismatch", name, FirstNonEmpty(info.Version, "unversioned"), CLIVersion))
		}
	}
	before, _ := otherRoots(root)
	for _, other := range before {
		switch state, path := candidateLauncher(other, name); state {
		case "unsafe":
			return pluginInstallResult{}, Hint(ErrConflict, "not installed: an unverifiable launcher at "+path+" may shadow this install (it sits on a path another user could change, so it was not run); inspect it, or have its owner remove it")
		case "accepted":
			return pluginInstallResult{}, Hint(ErrConflict, "not installed: "+path+" is earlier in the plugin roots and would still be the one that runs; its owner must update or remove it")
		}
	}
	dest := pluginLauncherPrefix + name
	if st, err := r.Lstat(dest); err == nil {
		if !st.Mode().IsRegular() {
			return pluginInstallResult{}, InputError("not installed: " + result.Path + " exists and is not a regular file; move it away first")
		}
		// The old launcher runs only if the walk's rule still holds for it
		// (the root has already passed the full-path check).
		if checkTrustedComponent(result.Path, os.Getuid(), false) != nil {
			result.ReplacedVersion = "not inspected (writable by others, or not yours)"
		} else if prev, err := readPluginMetadata(result.Path, name); err == nil {
			result.ReplacedVersion = FirstNonEmpty(prev.Version, "unversioned")
		} else {
			result.ReplacedVersion = "unreadable"
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return pluginInstallResult{}, err
	}
	if err := r.Rename(staged, dest); err != nil {
		return pluginInstallResult{}, err
	}
	// The rename is the commit point: from here the destination has changed.
	if err := syncPluginDir(r); err != nil {
		return result, Hint(errPluginDurability, fmt.Sprintf("capsulectl-%s %s was installed at %s (replacing %s; checks: %s; version: %s), but its directory could not be synced (%v)", name, FirstNonEmpty(result.Version, "unversioned"), result.Path, FirstNonEmpty(result.ReplacedVersion, "nothing"), strings.Join(result.Checks, ", "), result.VersionCheck, err))
	}
	return result, nil
}

// syncPluginDir is syncRootDir; a variable so a test can make it fail.
var syncPluginDir = syncRootDir

func syncRootDir(r *os.Root) error {
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// downloadTo streams url into w, hashing it, and fails past limit bytes or
// after pluginReadIdle without data.
func downloadTo(ctx context.Context, url string, w io.Writer, limit int64) (string, error) {
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("refusing %s: downloads use HTTPS", url)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := pluginDownloadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fs.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	idle := time.AfterFunc(pluginReadIdle, cancel)
	defer idle.Stop()
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(pluginReadIdle)
			total += int64(n)
			if total > limit {
				return "", fmt.Errorf("GET %s: over %d bytes, refusing to keep part of it", url, limit)
			}
			h.Write(buf[:n])
			if _, err := w.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("GET %s: no data for %s", url, pluginReadIdle)
			}
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var sumsLine = regexp.MustCompile(`^([0-9a-f]{64})  ([^\s]+)$`)

// sumFor reads sha256sum output and returns the one digest it names for file.
func sumFor(sums []byte, file string) (string, error) {
	found := ""
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if text == "" {
			continue
		}
		m := sumsLine.FindStringSubmatch(text)
		if m == nil {
			return "", fmt.Errorf("SHA256SUMS line %d is not `<sha256>  <file>`", line)
		}
		if m[2] == file {
			if found != "" {
				return "", fmt.Errorf("SHA256SUMS names %s twice", file)
			}
			found = m[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS does not name %s", file)
	}
	return found, nil
}

// ghAttestation runs the README's provenance check. ran is false when gh
// is not installed. Any other non-zero gh exit is a failed check, with no
// way around it; only a positive sign that gh could not run the check (it
// did not start, it timed out, or it could not build its Sigstore verifier)
// is reported as could-not-run.
func ghAttestation(ctx context.Context, asset, bundle string) (ran bool, err error) {
	gh, lookErr := exec.LookPath("gh")
	if lookErr != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ghVerifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "attestation", "verify", asset, "--bundle", bundle,
		"--repo", releaseRepo, "--signer-workflow", releaseRepo+"/.github/workflows/release.yml")
	cmd.Stdin = nil
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return true, nil
	}
	couldNotRun := func(why string) error {
		return Hint(errPluginFetch, "the provenance check could not run ("+why+"); fix gh's network or trust root and retry, or install without gh on PATH for a checksum-only install")
	}
	var exit *exec.ExitError
	text := strings.ToLower(string(out))
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return true, couldNotRun("gh timed out after " + ghVerifyTimeout.String())
	case !errors.As(runErr, &exit):
		return true, couldNotRun(runErr.Error())
	case strings.Contains(text, "error creating sigstore verifier"), strings.Contains(text, "no valid sigstore verifiers"):
		return true, couldNotRun(lastLine(string(out)))
	}
	return true, Hint(ErrConflict, "not installed: the provenance check failed ("+FirstNonEmpty(lastLine(string(out)), runErr.Error())+"): this file is not the release's build")
}

func pluginInstallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install book|deal|./PATH",
		Short: "Install a plugin into ~/.local/lib/capsulectl/plugins: book or deal from this release, or a launcher file (a path containing /)",
		Long: "Install a plugin on the per-user plugin root, ~/.local/lib/capsulectl/plugins.\n\n" +
			"`book` or `deal` fetches that first-party plugin from the release this capsulectl was built from, checks it against the release's SHA256SUMS and, when gh is installed, its build provenance (gh attestation verify), then installs it. A development build cannot do this: build the plugin and install the file.\n\n" +
			"An operand containing / is a launcher file (./capsulectl-book after make build, or a third-party plugin). Install runs its cli-plugin-metadata handshake and installs it under the name it gives.\n\n" +
			"Install never runs through sudo, and refuses a plugin root on a path another user could change. A first-party plugin must be the same version as this capsulectl unless --allow-version-mismatch is given. The output lists the checks that ran.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			home, err := requireOwnInstall()
			if err != nil {
				return err
			}
			operand := args[0]
			allow, _ := c.Flags().GetBool("allow-version-mismatch")
			if !strings.Contains(operand, "/") && !isFirstParty(operand) {
				hint := ""
				if strings.HasPrefix(operand, pluginLauncherPrefix) {
					hint = "; to install that file, give it as ./" + operand
				}
				return InputError("plugin install takes book, deal, or a launcher file path containing / (such as ./capsulectl-book)" + hint)
			}
			if strings.Contains(operand, "/") {
				if st, err := os.Stat(operand); err != nil || !st.Mode().IsRegular() {
					return InputError(operand + " is not a readable regular file")
				}
			} else if !isReleaseCore() {
				return InputError(fmt.Sprintf("this capsulectl is a development build (%s), so there is no release to fetch %s from: build it (make build), then run capsulectl plugin install ./capsulectl-%s", CLIVersion, operand, operand))
			} else if platform := runtime.GOOS + "/" + runtime.GOARCH; !slices.Contains(releasePlatforms, platform) {
				return InputError("there is no release build of the plugins for " + platform + " (releases ship " + strings.Join(releasePlatforms, ", ") + "); build it and install the file")
			}
			root, err := installRoot(home, true)
			if err != nil {
				return err
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				return err
			}
			defer r.Close()
			var staged string
			defer func() {
				if staged != "" {
					_ = r.Remove(staged)
				}
			}()
			var checks []string
			expected := ""
			if strings.Contains(operand, "/") {
				src, err := os.Open(operand)
				if err != nil {
					return InputError(operand + " cannot be read")
				}
				defer src.Close()
				if staged, err = stageInRoot(r, "", 0o755, func(w io.Writer) error { _, err := io.Copy(w, src); return err }); err != nil {
					return err
				}
				checks = []string{"handshake (cli-plugin-metadata)"}
			} else {
				expected = operand
				base, _ := c.Flags().GetString("release-url")
				if staged, checks, err = fetchFirstParty(c.Context(), r, root, base, operand); err != nil {
					return err
				}
			}
			result, err := finishInstall(r, root, staged, expected, allow, checks)
			if err == nil || errors.Is(err, errPluginDurability) {
				staged = ""
			}
			if err != nil {
				return err
			}
			return Output(c, result)
		},
	}
	cmd.Flags().Bool("allow-version-mismatch", false, "Install a first-party plugin whose version differs from this capsulectl's")
	cmd.Flags().String("release-url", pluginReleaseBase, "Base URL of the release downloads (tests)")
	_ = cmd.Flags().MarkHidden("release-url")
	return cmd
}

// fetchFirstParty downloads this release's plugin asset into the root,
// checks it against SHA256SUMS and, when gh is installed, its provenance.
func fetchFirstParty(ctx context.Context, r *os.Root, root, base, name string) (string, []string, error) {
	tag := CLIVersion
	asset := fmt.Sprintf("capsulectl-%s-plugin-%s-%s-%s", tag, name, runtime.GOOS, runtime.GOARCH)
	bundleName := "capsulectl-" + tag + ".sigstore.json"
	prefix := strings.TrimSuffix(base, "/") + "/" + tag + "/"
	var sums bytes.Buffer
	if _, err := downloadTo(ctx, prefix+"SHA256SUMS", &sums, maxPluginSums); errors.Is(err, fs.ErrNotExist) {
		return "", nil, InputError("release " + tag + " has no SHA256SUMS")
	} else if err != nil {
		return "", nil, Hint(errPluginFetch, err.Error())
	}
	want, err := sumFor(sums.Bytes(), asset)
	if err != nil {
		return "", nil, InputError("not installed: " + err.Error() + " (release " + tag + " may not ship plugins: they ship from the first release after capsule-cli#263)")
	}
	var got string
	staged, err := stageInRoot(r, "", 0o755, func(w io.Writer) error {
		var derr error
		got, derr = downloadTo(ctx, prefix+asset, w, maxPluginAsset)
		return derr
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil, InputError("release " + tag + " has no asset " + asset)
	case err != nil:
		return "", nil, Hint(errPluginFetch, err.Error())
	case got != want:
		_ = r.Remove(staged)
		return "", nil, Hint(ErrConflict, "not installed: "+asset+" does not match its SHA256SUMS line")
	}
	checks := []string{"sha256 (SHA256SUMS)"}
	if _, lookErr := exec.LookPath("gh"); lookErr != nil {
		return staged, append(checks, "provenance: not checked (gh not installed)"), nil
	}
	wantBundle, err := sumFor(sums.Bytes(), bundleName)
	if err != nil {
		_ = r.Remove(staged)
		return "", nil, InputError("not installed: " + err.Error())
	}
	var gotBundle string
	bundle, err := stageInRoot(r, ".sigstore.json", 0o600, func(w io.Writer) error {
		var derr error
		gotBundle, derr = downloadTo(ctx, prefix+bundleName, w, maxPluginBundle)
		return derr
	})
	if err == nil {
		defer r.Remove(bundle)
	}
	switch {
	case err != nil:
		_ = r.Remove(staged)
		return "", nil, Hint(errPluginFetch, err.Error())
	case gotBundle != wantBundle:
		_ = r.Remove(staged)
		return "", nil, Hint(ErrConflict, "not installed: "+bundleName+" does not match its SHA256SUMS line")
	}
	if _, err := ghAttestation(ctx, filepath.Join(root, staged), filepath.Join(root, bundle)); err != nil {
		_ = r.Remove(staged)
		return "", nil, err
	}
	return staged, append(checks, "provenance (gh attestation verify)"), nil
}

func pluginRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a plugin from ~/.local/lib/capsulectl/plugins",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			home, err := requireOwnInstall()
			if err != nil {
				return err
			}
			name := args[0]
			if !pluginNameGrammar.MatchString(name) {
				return InputError(fmt.Sprintf("%q is not a plugin name", name))
			}
			root, err := installRoot(home, false)
			if err != nil {
				return err
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				return err
			}
			defer r.Close()
			dest := pluginLauncherPrefix + name
			st, err := r.Lstat(dest)
			if errors.Is(err, fs.ErrNotExist) {
				return InputError("no plugin " + name + " in " + root)
			} else if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				return InputError(filepath.Join(root, dest) + " is not a regular file; remove it by hand")
			}
			if err := r.Remove(dest); err != nil {
				return err
			}
			out := map[string]any{"removed": filepath.Join(root, dest)}
			if d, err := r.Lstat(dest + ".d"); err == nil && d.IsDir() {
				out["kept"] = filepath.Join(root, dest+".d") + " (presentation modules; remove by hand if no longer needed)"
			}
			before, after := otherRoots(root)
			out["now_selected"] = "none"
			for _, other := range append(before, after...) {
				if state, path := candidateLauncher(other, name); state == "accepted" {
					out["now_selected"] = path
					break
				} else if state == "unsafe" {
					out["now_selected"] = path + " may be selected (not inspected: it sits on a path another user could change)"
					break
				}
			}
			if err := syncRootDir(r); err != nil {
				return Hint(errPluginDurability, fmt.Sprintf("capsulectl-%s was removed, but its directory could not be synced (%v)", name, err))
			}
			return Output(c, out)
		},
	}
}

// isPluginManagement reports whether args run `plugin install` or `plugin
// remove`, which need no dispatch: discovery is skipped for them, so no
// plugin's handshake runs before their own checks.
func isPluginManagement(root *cobra.Command, args []string) bool {
	found, _, err := root.Find(args)
	if err != nil || found == nil || found.Parent() == nil {
		return false
	}
	return found.Parent().Name() == "plugin" && (found.Name() == "install" || found.Name() == "remove")
}

// driftWarning is the stderr line for a first-party plugin whose version
// differs from a release core's, or "".
func driftWarning(info pluginInfo) string {
	if !isFirstParty(info.Name) || !isReleaseCore() || info.Version == CLIVersion {
		return ""
	}
	advice := "its owner must update it"
	if home := os.Getenv("HOME"); home != "" {
		c, _, _, err := canonicalPath(userPluginRoot(home))
		if dir, derr := filepath.EvalSymlinks(filepath.Dir(info.path)); err == nil && derr == nil && dir == c {
			advice = "run: capsulectl plugin install " + info.Name
		}
	}
	return fmt.Sprintf("capsulectl: warning: %s is %s, this capsulectl is %s; %s", info.path, FirstNonEmpty(info.Version, "unversioned"), CLIVersion, advice)
}

// leftoverInstalls lists temporary files a killed install left on the
// per-user root; discovery never runs them (no capsulectl- prefix).
func leftoverInstalls() []string {
	home := os.Getenv("HOME")
	if home == "" {
		return nil
	}
	root := userPluginRoot(home)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), pluginInstallPrefix) {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}
