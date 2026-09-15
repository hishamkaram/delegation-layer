package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/harnesscli"
)

func main() {
	os.Exit(harnesscli.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
