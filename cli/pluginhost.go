package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/action-state-group/capsule-emit/go/artifact"
	"github.com/action-state-group/checkpointed-local-log/go/cll"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The core keeps no evidence book (capsule-cli#236). A book profile's log is
// kept by the capsulectl-book plugin, and the deal commands are the
// capsulectl-deal plugin's. Both plugins are capsulectl binaries in their own
// right: the core command tree plus their own commands, built with
// NewPluginCommand. When the core meets a book profile, or a deal-only
// option, it runs the same command line in the plugin, which runs it with the
// backend the core lacks.

// The first-party plugins' names: the launcher capsulectl-<name> and the
// command it adds (`capsulectl book ...`, `capsulectl deal ...`).
const (
	BookPluginName = "book"
	DealPluginName = "deal"
)

// AliasRemovedIn is the release that removes the transitional aliases of
// commands that moved into the plugins (capsule-cli#238).
const AliasRemovedIn = "v0.2.0"

// ProfileEnv carries the selected profile into a plugin and into every
// command a plugin runs back in the core: `--profile` given anywhere on the
// command line wins over it.
const ProfileEnv = "CAPSULECTL_PROFILE"

// LogBackend opens the logs a plugin keeps for the profiles that name it as
// their log owner (Profile.LogOwner). A plugin registers one for its own
// name (RegisterLogBackend); a core process has none and runs the command in
// the owner instead.
type LogBackend interface {
	// Init creates the profile's book files for `store init`. It signs
	// nothing and opens no book.
	Init(ctx context.Context, p Profile) error
	// Open opens the profile's book for writing and holds its writer lock
	// until Release. artifacts is the profile's artifact store, which holds
	// every published capsule and every record's originals.
	Open(ctx context.Context, p Profile, artifacts ArtifactStore) (BookLog, error)
	// List is `cll list` on a book profile: read-only, no lock.
	List(ctx context.Context, p Profile, after, through uint64, limit int, all bool) (entries any, next uint64, err error)
	// WitnessStore keeps the delivery state of the book's checkpoints.
	WitnessStore(p Profile) cll.WitnessStateStore
}

// BookBackend is the backend of the book plugin's logs.
//
// Deprecated: the name goes with the plugin names in capsule-cli#428; use LogBackend.
type BookBackend = LogBackend

// OwnedLog is one open log of a plugin-kept profile.
type OwnedLog interface {
	// Publish seals the request as a capsule of the book, carrying its
	// record header, and commits that capsule as the record itself.
	Publish(ctx context.Context, r Request, key ed25519.PrivateKey) (Publication, error)
	// Append commits a capsule sealed elsewhere (`cll append`), already in
	// the artifact store, by wrapping it in a record.
	Append(ctx context.Context, record artifact.Record) (seq uint64, warning string, err error)
	// Checkpoint cuts a checkpoint; a non-empty service is the witness it is
	// then due for.
	Checkpoint(ctx context.Context, service string) (map[string]any, error)
	// Bundle builds an Evidence Bundle and puts it on record.
	Bundle(ctx context.Context, req BookBundleRequest) ([]byte, error)
	Release() error
}

// BookLog is one open book.
//
// Deprecated: use OwnedLog.
type BookLog = OwnedLog

// BookBundleRequest is `bundle`, `disclose` or `permalink` on a plugin-kept
// profile.
type BookBundleRequest struct {
	Root            string
	ClosureDepth    int
	Payloads        string
	Suppress        map[string]bool
	Disclose        bool
	AttachOriginals bool
	ProducerKey     ed25519.PublicKey
}

// logBackends are the backends this process serves, by plugin name. A plugin
// registers its own before building its tree; the core registers none.
var logBackends = map[string]LogBackend{}

// RegisterLogBackend installs the backend that serves the profiles whose log
// the plugin name keeps, in this binary.
func RegisterLogBackend(name string, b LogBackend) { logBackends[name] = b }

// SetBookBackend installs the backend that serves book profiles in this
// binary. The capsulectl-book plugin calls it before building its tree.
//
// Deprecated: use RegisterLogBackend(BookPluginName, b).
func SetBookBackend(b BookBackend) { RegisterLogBackend(BookPluginName, b) }

// backendFor is the backend serving p's log in this process, or nil when p's
// log is the core's or its owner's plugin runs elsewhere.
func backendFor(p Profile) LogBackend {
	owner := p.LogOwner()
	if owner == "" {
		return nil
	}
	return logBackends[owner]
}

// DealBundleRun builds `bundle`, `disclose` or `permalink` with --deal. The
// capsulectl-deal plugin sets it; while it is unset the command runs there.
var DealBundleRun func(c *cobra.Command, use string) error

// DealCommandGroup is the `deal` command group. The capsulectl-deal plugin
// sets it, so commands that run deal verbs themselves (`canary run`) run
// them in this process; while it is unset they run in that plugin.
var DealCommandGroup func() *cobra.Command

// DealTickCommand is `deal tick`, also mounted as `cll checkpoint cadence`.
// The capsulectl-deal plugin sets it; while it is unset the core mounts a
// command that runs `tick` there.
var DealTickCommand func() *cobra.Command

// errLogNeedsPlugin is reached only when a plugin-kept profile gets past the
// delegation below without a backend, which a command must never allow.
func errLogNeedsPlugin(p Profile) error {
	return InputError(fmt.Sprintf("profile %s keeps its log in the capsulectl-%s plugin; run this command through capsulectl with that plugin installed", p.Name, p.LogOwner()))
}

// delegateLog runs c's command line in the plugin that keeps p's log when this
// binary has no backend for it, and reports whether it did. Commands that
// write, list or bundle a profile's log call it first, before they read any
// input or open anything.
func delegateLog(c *cobra.Command, p Profile, args []string) (bool, error) {
	if !p.HasPluginLog() || backendFor(p) != nil {
		return false, nil
	}
	if p.LogOwner() == runningPlugin {
		// The owner's own process without a backend for its log: running the
		// command in the owner again would start this process again.
		return true, InputError(fmt.Sprintf("capsulectl-%s keeps the log of profile %s but registers no backend for it, so this command cannot run on it", runningPlugin, p.Name))
	}
	return true, ForwardToPlugin(c, p.LogOwner(), CommandLine(c, args))
}

// runningPlugin is the name of the plugin this process is (NewPluginCommand),
// or "" in the core.
var runningPlugin string

// CommandLine rebuilds the invocation c parsed: its path below the root,
// every flag set on it (persistent ones included, a repeated flag once per
// value), then args after "--".
func CommandLine(c *cobra.Command, args []string) []string {
	var path []string
	for cmd := c; cmd.HasParent(); cmd = cmd.Parent() {
		path = append([]string{cmd.Name()}, path...)
	}
	c.Flags().Visit(func(f *pflag.Flag) {
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range slice.GetSlice() {
				path = append(path, "--"+f.Name+"="+v)
			}
			return
		}
		path = append(path, "--"+f.Name+"="+f.Value.String())
	})
	if len(args) > 0 {
		path = append(append(path, "--"), args...)
	}
	return path
}

// ForwardToPlugin runs the first-party plugin name with args, or says it is
// not installed and where to install it.
func ForwardToPlugin(c *cobra.Command, name string, args []string) error {
	plugins, refused := discoverPluginsAndRefusals()
	return forwardDiscovered(c, name, args, plugins, refused)
}

// forwardToPluginAs is ForwardToPlugin with the profile the plugin inherits
// named instead of scanned out of args.
func forwardToPluginAs(c *cobra.Command, name string, args []string, profile string) error {
	plugins, refused := discoverPluginsAndRefusals()
	for _, info := range plugins {
		if info.Name == name {
			return execPluginAs(c, info, args, profile)
		}
	}
	return missingPluginError(name, refused)
}

// forwardDiscovered is ForwardToPlugin over one discovery's result.
func forwardDiscovered(c *cobra.Command, name string, args []string, plugins []pluginInfo, refused []refusedPlugin) error {
	for _, info := range plugins {
		if info.Name == name {
			return execPlugin(c, info, args)
		}
	}
	return missingPluginError(name, refused)
}

// missingPluginError says a first-party plugin is refused, with why, when
// discovery refused it (refused is that discovery's, so no launcher is run
// again), and otherwise that it is not installed and how to install it.
func missingPluginError(name string, refused []refusedPlugin) error {
	if err := refusedPluginError(name, refused); err != nil {
		return err
	}
	return InputError(fmt.Sprintf("this needs the capsulectl-%s plugin, which is not installed: install the plugin file from its release, built for this capsulectl, then run capsulectl plugin install ./capsulectl-%s", name, name))
}

// profileInArgs is the profile a plugin's raw command line selects: the last
// --profile value in it (pflag's own rule), else the inherited one.
func profileInArgs(args []string) string {
	name := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			i = len(args)
		case a == "--profile" && i+1 < len(args):
			name = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			name = strings.TrimPrefix(a, "--profile=")
		}
	}
	if name == "" {
		name = inheritedProfile()
	}
	return name
}

// inheritedProfile is ProfileEnv, read only inside a plugin run (where
// capsulectl set CAPSULECTL_PLUGIN_API for the plugin and its children), so
// a stray export in an operator's shell never selects a profile the command
// line did not name.
func inheritedProfile() string {
	if os.Getenv("CAPSULECTL_PLUGIN_API") == "" {
		return ""
	}
	return os.Getenv(ProfileEnv)
}

// aliasCommand is a transitional alias (capsule-cli#238): an old top-level
// name that now lives in a plugin. It prints a one-line notice naming the
// replacement on stderr and runs the replacement with the arguments
// unchanged; stdout and the exit code are the replacement's. pick, when
// set, chooses the plugin from the selected profile.
func aliasCommand(use, short string, target func(args []string) (plugin string, path []string, err error)) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			if slices.Contains(args, "--help") || slices.Contains(args, "-h") {
				return c.Help()
			}
			plugin, path, err := target(args)
			if err != nil {
				return err
			}
			replacement := "capsulectl " + plugin + " " + strings.Join(path, " ")
			old := strings.TrimPrefix(c.CommandPath(), c.Root().CommandPath()+" ")
			fmt.Fprintf(c.ErrOrStderr(), "capsulectl: `capsulectl %s` is deprecated; use `%s` (the alias is removed in %s)\n", old, replacement, AliasRemovedIn)
			return ForwardToPlugin(c, plugin, append(path, args...))
		},
	}
}

// aliasShort is an alias's help line: deprecated, its replacement, and the
// release that removes it.
func aliasShort(replacement string) string {
	return fmt.Sprintf("Deprecated: use `capsulectl %s`; removed in %s", replacement, AliasRemovedIn)
}

// transitionalAliases are the old entry points of commands that moved into
// the plugins, each forwarding with its arguments unchanged.
func transitionalAliases() []*cobra.Command {
	fixed := func(plugin string, path ...string) func([]string) (string, []string, error) {
		return func([]string) (string, []string, error) { return plugin, path, nil }
	}
	// request and respond go where they did before: a sqlite (deal)
	// profile's to capsulectl-deal, any other profile's to capsulectl-book.
	byProfile := func(verb string) func([]string) (string, []string, error) {
		return func(args []string) (string, []string, error) {
			name := profileInArgs(args)
			if name == "" {
				return BookPluginName, []string{verb}, nil
			}
			p, err := LoadProfile(name)
			if err != nil {
				return "", nil, Hint(ErrInput, err.Error())
			}
			if p.Type == "sqlite" {
				return DealPluginName, []string{verb}, nil
			}
			return BookPluginName, []string{verb}, nil
		}
	}
	return []*cobra.Command{
		aliasCommand("close", aliasShort("book close"), fixed(BookPluginName, "close")),
		aliasCommand("reconcile", aliasShort("book reconcile"), fixed(BookPluginName, "reconcile")),
		aliasCommand("request", aliasShort("book request (a deal profile: capsulectl deal request)"), byProfile("request")),
		aliasCommand("respond", aliasShort("book respond (a deal profile: capsulectl deal respond)"), byProfile("respond")),
		aliasCommand("compose", aliasShort("deal compose"), fixed(DealPluginName, "compose")),
		// `result build` is the core's now (capsule-cli#324); only `result
		// open` still forwards.
		resultCommand(),
		aliasCommand("report", aliasShort("book report"), fixed(BookPluginName, "report")),
		aliasCommand("backfill", aliasShort("book backfill"), fixed(BookPluginName, "backfill")),
	}
}

// firstPartyPlaceholder stands in for a first-party plugin that is not
// installed, or that discovery refused, so `capsulectl book ...` says what to
// install or why it is refused instead of reporting an unknown command.
func firstPartyPlaceholder(name, short string, refused []refusedPlugin) *cobra.Command {
	state := "not installed"
	if refusedPluginError(name, refused) != nil {
		state = "refused: see capsulectl plugin ls"
	}
	return &cobra.Command{
		Use:                name,
		Short:              short + " (plugin capsulectl-" + name + ", " + state + ")",
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return missingPluginError(name, refused)
		},
	}
}

// refusedPluginError is the refusal of a capsulectl-<name> launcher that
// discovery found and did not wire up, with its reason (which names the fix,
// such as the chmod for a group-writable ~/.local), or nil when none was
// refused: a refused plugin is installed, so "not installed" would mislead.
func refusedPluginError(name string, refused []refusedPlugin) error {
	for _, r := range refused {
		if filepath.Base(r.Path) == pluginLauncherPrefix+name {
			return InputError(fmt.Sprintf("the capsulectl-%s plugin at %s is refused: %s", name, terminalSafe(r.Path), terminalSafe(r.Reason)))
		}
	}
	return nil
}

// terminalSafe escapes the control characters in s, and the format and
// line or paragraph separator characters (a right-to-left override can
// reorder what a line shows). A refused launcher's path, and the paths its
// refusal names, may be written by the very user the refusal is about, who
// must not reach the operator's terminal with escape sequences or line
// breaks.
func terminalSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			if r > 0xffff {
				fmt.Fprintf(&b, "\\U%08x", r)
			} else if r > 0xff {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				fmt.Fprintf(&b, "\\x%02x", r)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// refusedPluginPlaceholder answers a refused launcher's name with its
// refusal, where cobra would say only "unknown command".
func refusedPluginPlaceholder(name string, refused []refusedPlugin) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              "(plugin capsulectl-" + name + ", refused: see capsulectl plugin ls)",
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return refusedPluginError(name, refused)
		},
	}
}

// PluginMetadata is what a first-party plugin binary answers to the
// cli-plugin-metadata handshake.
type PluginMetadata struct {
	Name        string
	Vendor      string
	Subcommands []string
}

// NewCommandWith is the capsulectl tree with cmds added at the top level, in
// the same process. Each replaces any core command of the same name: a
// transitional alias, or the plugin's own dispatch. It discovers no plugin.
// Tests of plugin commands run against it.
func NewCommandWith(cmds ...*cobra.Command) *cobra.Command {
	return newCommand(false, cmds)
}

// NewPluginCommand is a first-party plugin binary's tree: NewCommandWith,
// named capsulectl-<name>, answering the cli-plugin-metadata handshake.
func NewPluginCommand(meta PluginMetadata, cmds ...*cobra.Command) *cobra.Command {
	runningPlugin = meta.Name
	root := newCommand(false, cmds)
	root.Use = pluginLauncherPrefix + meta.Name
	// It is reached as `capsulectl <name>`, so its help and its messages
	// name its commands that way: `capsulectl deal history`.
	root.Annotations = map[string]string{cobra.CommandDisplayNameAnnotation: "capsulectl " + meta.Name}
	root.Version = FirstNonEmpty(PluginVersion, CLIVersion)
	root.SetVersionTemplate(root.Use + " {{.Version}} (commit " + FirstNonEmpty(PluginCommit, CLICommit) + ")\n")
	handshake := &cobra.Command{Use: "cli-plugin-metadata", Hidden: true, Args: NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return writePlainJSON(c, pluginInfo{Name: meta.Name, Vendor: meta.Vendor, Version: root.Version, CoreVersion: CLIVersion, PluginAPI: PluginAPI, Subcommands: meta.Subcommands})
	}}
	root.AddCommand(handshake)
	return root
}

// replaceCommands adds cmds to root, removing any command already there
// under the same name first.
func replaceCommands(root *cobra.Command, cmds []*cobra.Command) {
	for _, cmd := range cmds {
		for _, existing := range root.Commands() {
			if existing.Name() == cmd.Name() {
				root.RemoveCommand(existing)
			}
		}
		root.AddCommand(cmd)
	}
}

// writePlainJSON writes v as one line of plain JSON: the handshake's own format,
// not a capsule-cli-result.
func writePlainJSON(c *cobra.Command, v any) error {
	return json.NewEncoder(c.OutOrStdout()).Encode(v)
}
