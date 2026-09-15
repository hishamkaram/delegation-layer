// Command delegate runs the fixed Codex candidate through the normal app.
// It is never part of production builds or release artifacts.
package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/provider/codex"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdout, os.Stderr, app.Dependencies{PrepareCandidate: codex.PrepareCandidate}))
}
