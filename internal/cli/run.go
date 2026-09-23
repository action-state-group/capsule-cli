package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// actionstatePluginName is the plugin `run` dispatches to. The base binary
// carries no licence logic of its own (PLUGINS.md rule, restated by this
// task's constraints): it only asks whether a plugin named `actionstate`
// discovers and handshakes cleanly on the trusted plugin roots. A plugin
// that is genuinely absent and a plugin that refuses its own handshake
// because it is unlicensed are indistinguishable from here -- both mean
// discoverPlugins finds nothing named actionstate -- so one message covers
// both causes without the base binary ever inspecting a licence itself.
const actionstatePluginName = "actionstate"

// pluginRequiredError names a required plugin that discovery did not find.
// Its message is a fixed, static string carrying no path, secret, or profile
// detail -- like inputFileError and schemaLoadError, SafeError surfaces it in
// full instead of collapsing it to the generic ErrInput text, because this is
// the one case the task requires to reach the operator verbatim.
type pluginRequiredError struct{ message string }

func (e *pluginRequiredError) Error() string { return e.message }

func pluginRequiredErr(message string) error {
	return errors.Join(ErrInput, &pluginRequiredError{message: message})
}

// runCommand is a reserved base-binary command name (registered before
// addPluginCommands, so it is never shadowed by a discovered launcher): the
// base binary ships ONLY this dispatch. `--dry-run` and every other flag
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
