package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/harnesscli"
)

func main() {
	os.Exit(harnesscli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
