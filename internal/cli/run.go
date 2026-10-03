//go:build actionstate

package cli

import "github.com/spf13/cobra"

// actionstatePluginName is the plugin `run` dispatches to. The base binary
// carries no licence logic of its own (PLUGINS.md rule, restated by this
// task's constraints): it only asks whether a plugin named `actionstate`
// discovers and handshakes cleanly on the trusted plugin roots. A plugin
// that is genuinely absent and a plugin that refuses its own handshake
// because it is unlicensed are indistinguishable from here -- both mean
// discoverPlugins finds nothing named actionstate -- so one message covers
// both causes without the base binary ever inspecting a licence itself.
const actionstatePluginName = "actionstate"

// runCommand is a reserved base-binary command name (registered before
// addPluginCommands, so it is never shadowed by a discovered launcher): a build
// with the actionstate tag ships ONLY this dispatch. The default build carries
// run_default.go instead. `--dry-run` and every other flag
// belong to the actionstate plugin, never parsed here.
func runCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "run",
		Short:              "Dispatch to the Action State plugin (base binary ships dispatch only; requires the actionstate plugin)",
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			for _, info := range discoverPlugins() {
				if info.Name == actionstatePluginName {
					return execPlugin(c, info, args)
				}
			}
			return pluginRequiredErr("run requires the Action State plugin and a licence; see the actionstate plugin's own documentation for how to install and license it")
		},
	}
}
