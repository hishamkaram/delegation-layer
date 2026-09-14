package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorcli"
)

func main() {
	os.Exit(contributorcli.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
