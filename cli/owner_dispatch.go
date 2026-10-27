package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Commands a plugin extends with flags of its own (capsule-cli#428) cannot be
// parsed by the core when the selected profile's log is a plugin's: the flags
// are the plugin's. They take their raw command line, find the profile it
// selects, and hand the line to the plugin that keeps the profile's log when
// this process cannot serve it. Only then do they parse their flags.

// commandPath is c's path below the root, as forwarded argv starts.
func commandPath(c *cobra.Command) []string {
	var path []string
	for cmd := c; cmd.HasParent(); cmd = cmd.Parent() {
		path = append([]string{cmd.Name()}, path...)
	}
	return path
}

// helpRequested reports a --help or -h before any "--".
func helpRequested(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--help", "-h":
			return true
		}
	}
	return false
}

// routeToOwner runs c's raw command line in the plugin that keeps the log of
// the profile it selects, and reports whether it did. It does not when no
// profile is selected or it cannot be loaded (the command then says so itself,
// after parsing), when the core keeps the log, or when this process serves the
// owner's logs: it is the owner, or has its backend.
func routeToOwner(c *cobra.Command, args []string) (bool, error) {
	name := profileInArgs(args)
	if name == "" {
		return false, nil
	}
	p, err := LoadProfile(name)
	if err != nil {
		return false, nil
	}
	owner := p.LogOwner()
	if owner == "" || owner == runningPlugin || backendFor(p) != nil {
		return false, nil
	}
	return true, ForwardToPlugin(c, owner, append(commandPath(c), args...))
}

// ownerRouted makes cmd take its flags only after routeToOwner has declined.
func ownerRouted(cmd *cobra.Command) *cobra.Command {
	run, check := cmd.RunE, cmd.Args
	cmd.Args, cmd.DisableFlagParsing = nil, true
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if helpRequested(args) {
			return c.Help()
		}
		if done, err := routeToOwner(c, args); done {
			return err
		}
		// ParseFlags does nothing while DisableFlagParsing is set.
		c.DisableFlagParsing = false
		err := c.ParseFlags(args)
		c.DisableFlagParsing = true
		if err != nil {
			return flagError(c, err)
		}
		rest := c.Flags().Args()
		if check != nil {
			if err := check(c, rest); err != nil {
				return err
			}
		}
		return run(c, rest)
	}
	return cmd
}

// BundleExtension adds a plugin's flags and behavior to `bundle`, `disclose`
// and `permalink` in the plugin's own process.
type BundleExtension struct {
	// Flags defines the plugin's flags on the command named by use. It must
	// skip a flag the command already has.
	Flags func(f *pflag.FlagSet, use string)
	// Run runs first, with the flags parsed. When it reports handled, the
	// command is done and returns its error.
	Run func(c *cobra.Command, use string) (handled bool, err error)
}

var bundleExtensions []BundleExtension

// ExtendBundle registers ext. A plugin calls it before building its tree.
func ExtendBundle(ext BundleExtension) { bundleExtensions = append(bundleExtensions, ext) }

type graft struct {
	path []string
	cmd  *cobra.Command
}

var grafts []graft

// GraftCommand puts cmd under the core command at path (such as "cll",
// "checkpoint"), replacing a child of the same name. A plugin calls it before
// building its tree; cmd is used by the one tree the process builds. A path
// that names no core command is a programming error.
func GraftCommand(path []string, cmd *cobra.Command) {
	grafts = append(grafts, graft{append([]string(nil), path...), cmd})
}

// applyGrafts mounts the registered grafts on root.
func applyGrafts(root *cobra.Command) {
	for _, g := range grafts {
		parent := root
		for _, name := range g.path {
			var next *cobra.Command
			for _, child := range parent.Commands() {
				if child.Name() == name {
					next = child
				}
			}
			if next == nil {
				panic(fmt.Sprintf("GraftCommand: no core command %q on the path %s", name, strings.Join(g.path, " ")))
			}
			parent = next
		}
		replaceCommands(parent, []*cobra.Command{g.cmd})
	}
}
