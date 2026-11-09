package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/tuiprovider"
)

func main() {
	os.Exit(tuiprovider.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
