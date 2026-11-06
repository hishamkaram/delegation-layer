package main

import (
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	if code, handled := pueue.RunRepairEditorMode(args); handled {
		return code
	}
	return app.RunRunner(args, stdout, stderr, app.Dependencies{PublisherVersion: version})
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
