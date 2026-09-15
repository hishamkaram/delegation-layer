package main

import (
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/inspectionfixture"
)

func main() {
	if err := inspectionfixture.RunHelper(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "inspection fixture helper failed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
