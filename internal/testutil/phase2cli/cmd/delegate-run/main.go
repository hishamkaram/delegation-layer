package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2cli"
)

func main() {
	os.Exit(phase2cli.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
