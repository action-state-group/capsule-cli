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
		fmt.Fprintln(os.Stderr, "capsule: "+cli.SafeError(err))
		os.Exit(cli.ExitCode(err))
	}
}
