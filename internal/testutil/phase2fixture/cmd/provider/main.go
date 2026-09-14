package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2fixture"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--child" {
		if err := phase2fixture.RunChild(os.Args[2], os.Stdin, os.Stdout, os.Stderr); err != nil {
			finish(err)
		}
		return
	}
	if len(os.Args) >= 2 {
		if err := phase2fixture.RunProviderArgs(os.Args[1], os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			finish(err)
		}
		return
	}
	if _, err := fmt.Fprintln(os.Stderr, "usage: provider /absolute/path/to/phase2-fixture.json [literal-argv ...]"); err != nil {
		os.Exit(1)
	}
	os.Exit(2)
}

func finish(err error) {
	var exitErr *phase2fixture.ExitError
	if errors.As(err, &exitErr) && exitErr.Code >= 1 && exitErr.Code <= 125 {
		os.Exit(exitErr.Code)
	}
	if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
		os.Exit(1)
	}
	os.Exit(1)
}
