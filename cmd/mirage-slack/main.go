package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	mirageslack "github.com/mashiike/mirage-slack"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mirageslack.Run(ctx, os.Args[1:]); err != nil {
		slog.Error("mirage-slack exited with error", "error", err)
		os.Exit(1)
	}
}
