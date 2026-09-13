package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	root := fs.String("root", "", "store root directory")
	taskID := fs.String("task-id", "", "32 lowercase hex task ID")

	if pErr := fs.Parse(args); pErr != nil {
		return 2
	}
	if *root == "" || *taskID == "" {
		return 2
	}

	s, sErr := taskdir.OpenStore(*root)
	if sErr != nil {
		fmt.Fprintf(os.Stderr, "error opening store: %v\n", sErr)
		return 1
	}
	defer closeStore(s)

	td, tErr := s.OpenTask(*taskID)
	if tErr != nil {
		fmt.Fprintf(os.Stderr, "error opening task: %v\n", tErr)
		return 1
	}
	defer closeTask(td)

	insp, inspErr := td.Inspect()
	if inspErr != nil {
		fmt.Fprintf(os.Stderr, "error inspecting task: %v\n", inspErr)
		return 1
	}

	return printJSON(insp)
}
