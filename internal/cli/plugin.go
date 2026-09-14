package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// pluginAPI is the handshake contract version. A plugin's cli-plugin-metadata
// must advertise exactly this; any other value is a loud dispatch failure rather
// than a silently-ignored plugin.
const pluginAPI = "cli-plugin/v1"

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

// verifyTrustedPath rejects a launcher whose real path, or any directory between
// it and the matched trusted root (inclusive), is group-writable, other-writable,
// or owned by neither the current user nor root; and rejects any launcher whose
// resolved path escapes the trusted roots (so a symlink cannot redirect discovery
// to an untrusted target) or is not a regular file. The root's own parents are
// system directories, trusted by definition, so the walk stops at the root.
func verifyTrustedPath(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	if info, err := os.Lstat(resolved); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", resolved)
	}
	realRoot := ""
	for _, root := range trustedPluginRoots() {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if resolved == rr || strings.HasPrefix(resolved, rr+string(os.PathSeparator)) {
			realRoot = rr
			break
		}
	}
	if realRoot == "" {
		return fmt.Errorf("%s resolves to %s, outside the trusted plugin roots", path, resolved)
	}
	// Walk from the launcher up to and including the trusted root: the root's own
	// parents are system directories trusted by definition, but any directory
	// BETWEEN the root and the launcher (or the launcher itself) that is
	// group/other-writable or owned by neither the user nor root defeats the trust.
	uid := os.Getuid()
	for p := resolved; ; {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot read ownership of %s", p)
		}
		if int(stat.Uid) != uid && stat.Uid != 0 {
			return fmt.Errorf("%s is owned by neither the current user nor root", p)
		}
		if mode := info.Mode(); mode&0o020 != 0 {
			return fmt.Errorf("%s is group-writable", p)
		} else if mode&0o002 != 0 {
			return fmt.Errorf("%s is other-writable", p)
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
	return nil
}

type pluginInfo struct {
	Name        string   `json:"name"`
	Vendor      string   `json:"vendor"`
	Version     string   `json:"version"`
	PluginAPI   string   `json:"plugin_api"`
	Subcommands []string `json:"subcommands,omitempty"`
	path        string
}

// pluginMetadata runs the launcher's cli-plugin-metadata handshake. A launcher
// that does not answer, answers with the wrong command name, or advertises a
// different plugin_api is refused (returned as an error) rather than trusted.
func pluginMetadata(path, name string) (pluginInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "cli-plugin-metadata").Output()
	if err != nil {
		return pluginInfo{}, fmt.Errorf("%s: cli-plugin-metadata handshake failed: %w", name, err)
	}
	var info pluginInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return pluginInfo{}, fmt.Errorf("%s: cli-plugin-metadata is not JSON: %w", name, err)
	}
	if info.PluginAPI != pluginAPI {
		return pluginInfo{}, fmt.Errorf("%s: plugin_api %q does not match %q", name, info.PluginAPI, pluginAPI)
	}
	if info.Name != name {
		return pluginInfo{}, fmt.Errorf("%s: launcher name mismatch (metadata says %q)", name, info.Name)
	}
	info.path = path
	return info, nil
}

// discoverPlugins scans the trusted roots for capsulectl-<name> launchers, in root
// order (an earlier root shadows a later one for the same name), keeping only those
// that pass the trusted-path check and a valid metadata handshake.
func discoverPlugins() []pluginInfo {
	var plugins []pluginInfo
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
			if verifyTrustedPath(path) != nil {
				continue
			}
			info, err := pluginMetadata(path, name)
			if err != nil {
				continue
			}
			seen[name] = true
			plugins = append(plugins, info)
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	return plugins
}

// pluginCommand wraps a discovered launcher as a cobra command that execs it,
// passing the remaining args through unchanged (flag parsing disabled so the
// plugin owns its own flags). A plugin never shadows a core command: cobra does
// NOT ignore a duplicate Use, so addPluginCommands explicitly skips any name
// already reserved by a core command (and by help/completion).
func pluginCommand(info pluginInfo) *cobra.Command {
	return &cobra.Command{
		Use:                info.Name,
		Short:              fmt.Sprintf("%s (plugin · %s)", firstNonEmpty(pluginShort(info), "external plugin"), firstNonEmpty(info.Vendor, "unknown vendor")),
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			// Re-verify at dispatch time so a swap of the launcher between discovery
			// and now is caught. This closes the discovery->dispatch window, not the
			// residual verify->exec window (an exec via an O_PATH fd would be needed
			// for that); under the "current user or root" trust model the remaining
			// race is only against a same-user process.
			if err := verifyTrustedPath(info.path); err != nil {
				return inputError("refusing to run plugin from an untrusted path")
			}
			cmd := exec.CommandContext(c.Context(), info.path, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr()
			cmd.Env = append(os.Environ(), "CAPSULECTL_PLUGIN_API="+pluginAPI)
			if err := cmd.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return exit
				}
				return fmt.Errorf("plugin %s failed: %w", info.Name, err)
			}
			return nil
		},
	}
}

func pluginShort(info pluginInfo) string {
	if len(info.Subcommands) == 0 {
		return ""
	}
	return "subcommands: " + strings.Join(info.Subcommands, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// pluginGroup is the read-only `plugin` command group: `plugin ls` lists the
// discovered launchers. There is deliberately no install/enable/disable — the
// core never mutates plugin state; installation is an operator step onto a
// trusted root.
func pluginGroup() *cobra.Command {
	group := &cobra.Command{Use: "plugin", Short: "Inspect discovered capsulectl-* plugins (read-only)"}
	ls := &cobra.Command{Use: "ls", Short: "List trusted, handshake-valid plugins discovered on the plugin roots", Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			plugins := discoverPlugins()
			rows := make([]map[string]any, 0, len(plugins))
			for _, p := range plugins {
				rows = append(rows, map[string]any{"name": p.Name, "vendor": p.Vendor, "version": p.Version,
					"plugin_api": p.PluginAPI, "subcommands": p.Subcommands, "path": p.path})
			}
			return output(c, map[string]any{"roots": trustedPluginRoots(), "plugins": rows})
		}}
	group.AddCommand(ls)
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
	root.AddCommand(pluginGroup())
	reserved := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		reserved[c.Name()] = true
	}
	for _, info := range discoverPlugins() {
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
}
