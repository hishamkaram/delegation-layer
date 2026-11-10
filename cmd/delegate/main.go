package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	if parsed, err := app.ParseArguments(args); err == nil {
		switch parsed.Command {
		case "tui":
			return tui.Launch(parsed, app.Dependencies{PublisherVersion: version, Input: os.Stdin}, os.Stdin, stdout, stderr)
		case "scheduler":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			return app.RunContext(ctx, args, stdout, stderr, app.Dependencies{PublisherVersion: version, Input: os.Stdin})
		}
	}
	return app.Run(args, stdout, stderr, app.Dependencies{PublisherVersion: version, Input: os.Stdin})
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
