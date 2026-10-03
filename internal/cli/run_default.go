//go:build !actionstate

package cli

import "github.com/spf13/cobra"

// runUnavailable is what `run` says in the default build. The default build
// carries no plugin dispatch: the command exists only to keep the name
// reserved, so a discovered launcher can never take it (core commands are
// registered before addPluginCommands).
const runUnavailable = "run is not available in this build"

func runCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "run",
		Short:              runUnavailable,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return pluginRequiredErr(runUnavailable)
		},
	}
}
