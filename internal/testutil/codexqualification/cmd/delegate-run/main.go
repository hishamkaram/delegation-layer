// Command delegate-run qualifies the fixed Codex candidate through the normal runner.
// It has no process, evidence, or lifecycle implementation of its own.
package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/provider/codex"
)

func main() {
	os.Exit(app.RunRunner(os.Args[1:], os.Stdout, os.Stderr, app.Dependencies{PrepareProfile: codex.PrepareCandidate}))
}
