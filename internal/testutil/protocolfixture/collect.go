package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func handleCollectError(err error) int {
	fmt.Fprintf(os.Stderr, "collection: %v\n", err)
	switch {
	case errors.Is(err, task.ErrOutcomeConflict):
		return 4
	case errors.Is(err, task.ErrInvariantFault):
		return 5
	case errors.Is(err, task.ErrUncertainDurability):
		return 13
	case errors.Is(err, task.ErrNoSeal):
		return 14
	case errors.Is(err, task.ErrEvidenceFault), errors.Is(err, task.ErrIncompatiblePredicate):
		return 15
	case errors.Is(err, task.ErrLockBusy):
		return 16
	default:
		return 1
	}
}

func writeOutcomePayload(td *taskdir.TaskDir, outcome *task.OutcomeRecord) int {
	payload, err := td.OpenPayload(outcome)
	if err != nil {
		return handleCollectError(err)
	}
	_, copyErr := io.Copy(os.Stdout, payload)
	if err = errors.Join(copyErr, payload.Close()); err != nil {
		return handleCollectError(err)
	}
	return 0
}

func writeOutcomeControl(outcome *task.OutcomeRecord, cleanupErr, pubErr error) int {
	status := "published"
	if pubErr != nil {
		status = "conflict"
	}
	result := map[string]any{"status": status, "task_id": outcome.TaskID, "verdict": outcome.Verdict, "payload_digest": outcome.Payload.SHA256, "outcome": outcome}
	if pubErr != nil {
		result["error"] = pubErr.Error()
	}
	if cleanupErr != nil {
		result["cleanup_error"] = cleanupErr.Error()
	}
	return printJSON(result)
}

func collectionResult(td *taskdir.TaskDir, outcome *task.OutcomeRecord, cleanupErr, pubErr error, rawOutput bool) int {
	if outcome != nil {
		var code int
		if rawOutput {
			code = writeOutcomePayload(td, outcome)
		} else {
			code = writeOutcomeControl(outcome, cleanupErr, pubErr)
		}
		if code != 0 {
			return code
		}
	}
	if pubErr != nil {
		return handleCollectError(pubErr)
	}
	if cleanupErr != nil {
		return handleCollectError(cleanupErr)
	}
	if outcome == nil {
		return handleCollectError(errors.New("collection returned no outcome"))
	}
	return 0
}

func runCollect(args []string) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	root := fs.String("root", "", "store root")
	id := fs.String("task-id", "", "task ID")
	wrong := fs.Bool("wrong-predicate", false, "request an unavailable predicate version")
	raw := fs.Bool("raw-output", false, "stream exact terminal payload instead of control JSON")
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
	predicate := task.FixturePredicateRef()
	if *wrong {
		predicate.Version = "unavailable"
	}
	outcome, cleanupErr, pubErr := td.Collect(predicate)
	return collectionResult(td, outcome, cleanupErr, pubErr, *raw)
}
