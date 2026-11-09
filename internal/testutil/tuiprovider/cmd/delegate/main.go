package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/tuiprovider"
)

func main() {
	os.Exit(tuiprovider.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
