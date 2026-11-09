package main

import (
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/fixture"
	"github.com/hishamkaram/delegation-layer/internal/testutil/tuiprovider/gate"
)

func main() {
	if err := gate.BeforeProvider(os.Args[1:]); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(2)
	}
	os.Exit(fixture.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
