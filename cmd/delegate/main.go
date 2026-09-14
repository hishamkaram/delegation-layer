package main

import (
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usageText = app.UsageText

func run(args []string, stdout, stderr io.Writer) int {
	return app.Run(args, stdout, stderr, app.Dependencies{PublisherVersion: version})
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
