// Command capsulectl-deal is the capsulectl-deal plugin: `capsulectl deal
// ...`, a deal profile's commands and its SQLite deal store. It is the
// capsulectl command tree with the deal commands added, so the core runs
// `bundle --deal` and `cll checkpoint cadence` here (capsule-cli#236).
package main

import (
	"context"
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
	// The error line on stderr, and an error object on stdout when the
	// command printed nothing (capsule-cli#345).
	code := cli.Run(ctx, cli.NewPluginCommand(plugin.DealMetadata, plugin.DealCommands()...), "capsulectl-deal")
	cancel()
	os.Exit(code)
}
