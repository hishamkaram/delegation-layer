package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// WaitResponse contains compact passive observations in requested ID order.
// Ready describes evidence readiness, not provider success or resumability.
type WaitResponse struct {
	Mode      string     `json:"mode"`
	Satisfied bool       `json:"satisfied"`
	Tasks     []WaitTask `json:"tasks"`
}

type WaitTask struct {
	TaskID      string                         `json:"task_id"`
	Admission   string                         `json:"admission"`
	Liveness    string                         `json:"liveness"`
	Publication string                         `json:"publication"`
	Ready       bool                           `json:"ready"`
	Readiness   tasklifecycle.PassiveReadiness `json:"readiness"`
	Status      string                         `json:"status,omitempty"`
	Failure     *DispatchFailureResponse       `json:"failure,omitempty"`
}

func waitTasks(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("wait")
	if err := validateWaitArguments(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	duration := a.Watch
	if duration == 0 {
		duration = 5 * time.Second // One passive sample still has a finite I/O bound.
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.registry())
	if err != nil {
		if a.Watch > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return commandResult{response: response, code: 3}
		}
		if _, statErr := os.Stat(root); errors.Is(statErr, os.ErrNotExist) {
			return waitFailure(response, "task_not_found", err)
		}
		return waitFailure(response, "task_observation_failed", err)
	}
	defer func() { mergeCommandClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	return watchWaitTasks(ctx, a, reader, response, time.Second)
}

// Each round reads every requested task, including after one becomes ready,
// so any-of includes all finished tasks observed in that round. One deadline
// bounds all rounds and reads; there is no per-task renewal.
func watchWaitTasks(ctx context.Context, a Arguments, reader tasklifecycle.PassiveTaskReader, response Response, poll time.Duration) commandResult {
	watch := &WaitResponse{Mode: "any", Tasks: make([]WaitTask, 0, len(a.WaitTaskIDs))}
	if a.WaitAll {
		watch.Mode = "all"
	}
	response.Wait = watch
	deadline := time.Now().Add(a.Watch)
	readCtx := ctx
	cancel := func() {}
	if a.Watch > 0 {
		readCtx, cancel = context.WithDeadline(ctx, deadline)
	}
	defer cancel()
	for _, id := range a.WaitTaskIDs {
		watch.Tasks = append(watch.Tasks, WaitTask{TaskID: id, Admission: "unknown", Liveness: "undetermined", Publication: "unknown", Readiness: tasklifecycle.PassivePending})
	}
	for {
		ready, fault := observeWaitRound(readCtx, reader, watch)
		if fault != nil {
			if a.Watch > 0 && errors.Is(readCtx.Err(), context.DeadlineExceeded) {
				return commandResult{response: response, code: 3}
			}
			response.TaskID = fault.taskID
			return waitFailure(response, fault.code, fault.err)
		}
		watch.Satisfied = ready > 0 && (!a.WaitAll || ready == len(a.WaitTaskIDs))
		if watch.Satisfied {
			return commandResult{response: response}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return commandResult{response: response, code: 3}
		}
		if err := awaitWaitPoll(readCtx, deadline, poll); err != nil {
			if errors.Is(err, context.Canceled) {
				return waitFailure(response, "task_observation_failed", err)
			}
			return commandResult{response: response, code: 3}
		}
	}
}

type waitReadFault struct {
	taskID string
	code   string
	err    error
}

func observeWaitRound(ctx context.Context, reader tasklifecycle.PassiveTaskReader, watch *WaitResponse) (int, *waitReadFault) {
	ready := 0
	for index, prior := range watch.Tasks {
		facts, observed, err := tasklifecycle.ReadPassiveTaskObservation(ctx, reader, prior.TaskID)
		if err != nil {
			return 0, &waitReadFault{taskID: prior.TaskID, code: "task_observation_failed", err: err}
		}
		if !facts.Exists {
			return 0, &waitReadFault{taskID: prior.TaskID, code: "task_not_found", err: fmt.Errorf("task %s: %w", prior.TaskID, os.ErrNotExist)}
		}
		item := WaitTask{TaskID: prior.TaskID, Admission: observed.Admission.String(), Liveness: observed.Liveness.String(), Publication: observed.Publication.String(), Ready: observed.Ready, Readiness: observed.Readiness, Status: observed.Status}
		if observed.FailureCode != "" {
			item.Failure = &DispatchFailureResponse{Code: observed.FailureCode, Stage: "wait", NextAction: "inspect_task"}
		}
		watch.Tasks[index] = item
		if observed.Ready {
			ready++
		}
	}
	return ready, nil
}

func waitFailure(response Response, code string, err error) commandResult {
	response.Failure = &DispatchFailureResponse{Code: code, Stage: "wait", NextAction: "stop_and_report"}
	if code == "task_observation_failed" {
		err = waitEvidenceError{cause: err}
	}
	return failed(response, err, 1)
}

func writeHumanWait(output *strings.Builder, wait *WaitResponse) {
	if wait == nil {
		return
	}
	fmt.Fprintf(output, "wait mode=%s satisfied=%t\n", wait.Mode, wait.Satisfied)
	for _, item := range wait.Tasks {
		fmt.Fprintf(output, "%s ready=%t readiness=%s admission=%s liveness=%s publication=%s", item.TaskID, item.Ready, item.Readiness, item.Admission, item.Liveness, item.Publication)
		if item.Status != "" {
			fmt.Fprintf(output, " status=%s", item.Status)
		}
		if item.Failure != nil {
			fmt.Fprintf(output, " failure_code=%s", item.Failure.Code)
		}
		output.WriteByte('\n')
	}
}

// Keep arbitrary storage/interpreter diagnostics out of control responses while
// preserving typed causes for callers and cleanup retention.
type waitEvidenceError struct{ cause error }

func (waitEvidenceError) Error() string {
	return "Delegate could not validate task evidence; inspect the task records."
}

func (e waitEvidenceError) Unwrap() error { return e.cause }

// Poll timing owns its timer independently from evidence classification.
func awaitWaitPoll(ctx context.Context, deadline time.Time, poll time.Duration) error {
	timer := time.NewTimer(min(poll, time.Until(deadline)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
