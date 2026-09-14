package main

import (
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	return app.RunRunner(args, stdout, stderr, app.Dependencies{PublisherVersion: version})
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
