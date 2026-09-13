package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func runScavenge(args []string) int {
	fs := flag.NewFlagSet("scavenge", flag.ContinueOnError)
	root := fs.String("root", "", "store root directory")

	if pErr := fs.Parse(args); pErr != nil {
		return 2
	}
	if *root == "" {
		return 2
	}

	s, sErr := taskdir.OpenStore(*root)
	if sErr != nil {
		fmt.Fprintf(os.Stderr, "error opening store: %v\n", sErr)
		return 1
	}
	defer closeStore(s)

	cleaned, scErr := s.Scavenge()
	if scErr != nil {
		fmt.Fprintf(os.Stderr, "error during scavenge: %v\n", scErr)
		return 1
	}

	res := map[string]any{
		"status":        "scavenged",
		"cleaned_count": cleaned,
	}
	return printJSON(res)
}
