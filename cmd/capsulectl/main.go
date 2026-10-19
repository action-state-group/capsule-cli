package main

import (
	"context"
	"fmt"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := cli.NewCommand().ExecuteContext(ctx); err != nil {
		// Empty when a plugin this ran has already said why.
		if msg := cli.SafeError(err); msg != "" {
			fmt.Fprintln(os.Stderr, "capsulectl: "+msg)
		}
		os.Exit(cli.ExitCode(err))
	}
}
