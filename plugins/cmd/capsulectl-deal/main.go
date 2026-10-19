// Command capsulectl-deal is the capsulectl-deal plugin: `capsulectl deal
// ...`, a deal profile's commands and its SQLite deal store. It is the
// capsulectl command tree with the deal commands added, so the core runs
// `bundle --deal` and `cll checkpoint cadence` here (capsule-cli#236).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/action-state-group/capsule-cli/internal/cli"
	"github.com/action-state-group/capsule-cli/plugins/internal/plugin"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	plugin.InstallDealHooks()
	if err := cli.NewPluginCommand(plugin.DealMetadata, plugin.DealCommands()...).ExecuteContext(ctx); err != nil {
		// Empty when a plugin this ran has already said why.
		if msg := cli.SafeError(err); msg != "" {
			fmt.Fprintln(os.Stderr, "capsulectl-deal: "+msg)
		}
		os.Exit(cli.ExitCode(err))
	}
}
