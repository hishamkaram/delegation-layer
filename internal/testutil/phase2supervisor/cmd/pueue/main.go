package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2supervisor"
)

func main() {
	// The helper exits only after its finite command and completion receipt are
	// written. It never signals or spawns another process.
	//
	// os.Exit is intentionally kept in this tiny command boundary so the
	// library remains directly testable without process lifecycle controls.
	if code := phase2supervisor.Main(); code != 0 {
		os.Exit(code)
	}
}
