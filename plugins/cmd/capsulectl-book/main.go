// Command capsulectl-book is the capsulectl-book plugin: `capsulectl book
// ...`, and the evidence book behind every book profile. It is the capsulectl
// command tree with an evidence book backend, so the core runs a book
// profile's publish, cll and bundle commands here (capsule-cli#236).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/action-state-group/capsule-cli/cli"
	"github.com/action-state-group/capsule-cli/plugins/internal/plugin"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cli.SetBookBackend(plugin.BookBackend{})
	// The error line on stderr, and an error object on stdout when the
	// command printed nothing (capsule-cli#345).
	code := cli.Run(ctx, cli.NewPluginCommand(plugin.BookMetadata, append(plugin.BookCommands(), plugin.BookAliases()...)...), "capsulectl-book")
	cancel()
	os.Exit(code)
}
