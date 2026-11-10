package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/tuiprovider"
	"github.com/hishamkaram/delegation-layer/internal/tui"
)

func main() {
	tui.HoldHangupUntilExit(os.Args[1:])
	os.Exit(tuiprovider.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
