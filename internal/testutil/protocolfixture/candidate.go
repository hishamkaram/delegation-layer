package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// candidate deliberately competes with the pure predicate in tests. The normal
// collect command has no caller-controlled answer or verdict.
func runCandidate(args []string) int {
	fs := flag.NewFlagSet("candidate", flag.ContinueOnError)
	root := fs.String("root", "", "store root")
	id := fs.String("task-id", "", "task ID")
	basename := fs.String("file", "result.txt", "candidate payload basename")
	content := fs.String("content", "candidate content", "candidate payload")
	verdict := fs.String("verdict", task.VerdictCommitted, "candidate verdict")
	recordFile := fs.String("candidate-record", "", "bounded explicit candidate outcome fixture")
	checkpoints := checkpointFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *root == "" || *id == "" {
		return 2
	}
	s, err := taskdir.OpenStore(*root)
	if err != nil {
		return handleCollectError(err)
	}
	defer closeStore(s)
	s.SetFaultInjector(checkpoints)
	td, err := s.OpenTask(*id)
	if err != nil {
		return handleCollectError(err)
	}
	defer closeTask(td)
	if *recordFile != "" {
		data, readErr := readControlFile(*recordFile)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, readErr)
			return 1
		}
		var candidate task.OutcomeRecord
		if readErr = task.DecodeStrict(data, &candidate); readErr != nil {
			fmt.Fprintln(os.Stderr, readErr)
			return 1
		}
		outcome, cleanupErr, pubErr := td.PublishRecordCandidateForTest(&candidate, []byte(*content))
		return collectionResult(td, outcome, cleanupErr, pubErr, false)
	}
	outcome, cleanupErr, pubErr := td.PublishCandidateForTest(*verdict, *basename, []byte(*content), task.FixturePredicateRef())
	return collectionResult(td, outcome, cleanupErr, pubErr, false)
}
