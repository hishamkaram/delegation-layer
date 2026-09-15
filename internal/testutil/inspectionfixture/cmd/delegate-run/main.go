package main

import (
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/inspectionfixture"
)

func main() {
	os.Exit(inspectionfixture.RunRunner(os.Args[1:], os.Stdout, os.Stderr))
}
