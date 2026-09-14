package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/fixture"
)

func main() {
	os.Exit(fixture.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
