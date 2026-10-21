package main

import (
	"context"
	"github.com/action-state-group/capsule-cli/internal/cli"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// The error line on stderr, and an error object on stdout when the
	// command printed nothing (capsule-cli#345).
	code := cli.Run(ctx, cli.NewCommand(), "capsulectl")
	cancel()
	os.Exit(code)
}
