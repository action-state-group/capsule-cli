// Command capsulectl-book is the capsulectl-book plugin: `capsulectl book
// ...`, and the evidence book behind every book profile. It is the capsulectl
// command tree with an evidence book backend, so the core runs a book
// profile's publish, cll and bundle commands here (capsule-cli#236).
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
	cli.SetBookBackend(plugin.BookBackend{})
	if err := cli.NewPluginCommand(plugin.BookMetadata, append(plugin.BookCommands(), plugin.BookAliases()...)...).ExecuteContext(ctx); err != nil {
		// Empty when a plugin this ran has already said why.
		if msg := cli.SafeError(err); msg != "" {
			fmt.Fprintln(os.Stderr, "capsulectl-book: "+msg)
		}
		os.Exit(cli.ExitCode(err))
	}
}
