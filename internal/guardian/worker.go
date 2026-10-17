package guardian

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (g *Guardian) startAndRecordWorker(ctx context.Context, reader *lifetimeGuardianReader, bootID string) (<-chan error, error) {
	if err := g.options.StartWorker(g.options.Worker); err != nil {
		stopErr := g.stopReader(ctx, reader)
		recoveryErr := g.recoverWithCause(ctx, fmt.Errorf("start lifetime worker: %w", err))
		return nil, errors.Join(err, stopErr, recoveryErr)
	}
	g.workerStarted = true
	if g.options.Worker.Process != nil {
		g.workerPID = g.options.Worker.Process.Pid
	}
	workerRef, err := g.options.ObserveWorker(g.options.Worker)
	if err == nil {
		g.workerPID = workerRef.PID()
		err = g.scope.TrackOwnerProcess(workerRef)
	}
	waitDone := make(chan error, 1)
	g.workerWait = waitDone
	go func() {
		waitDone <- g.options.WaitWorker(g.options.Worker)
	}()
	if err == nil {
		err = g.persistWorkerIdentity(context.WithoutCancel(ctx), workerRef, bootID)
	}
	if err == nil {
		return waitDone, nil
	}
	cleanupCtx, cancel := g.cleanupContext(ctx)
	stopErr := g.stopReaderWithContext(cleanupCtx, reader)
	cancel()
	_, waitErr := g.awaitWorkerWait(context.WithoutCancel(ctx))
	waitRecordErr := g.persistWorkerWait(context.WithoutCancel(ctx))
	cleanupCtx, cancel = g.cleanupContext(ctx)
	defer cancel()
	recoveryErr := g.recoverWithCause(cleanupCtx, fmt.Errorf("record lifetime worker: %w", err))
	return nil, errors.Join(err, stopErr, waitErr, waitRecordErr, recoveryErr)
}

// persistWorkerIdentity retains native authority until the started worker's
// create-once identity is durably acknowledged. RecordLifetimeWorker supports
// exact replay, so retrying also resolves a write that committed before its
// durability barrier reported an error. The worker cannot receive its hello
// acknowledgement until this stage succeeds.
func (g *Guardian) persistWorkerIdentity(ctx context.Context, ref lifetime.ProcessRef, bootID string) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if err := g.recordWorker(ctx, ref, bootID); err == nil {
			return nil
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.WaitRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *Guardian) recordWorker(ctx context.Context, ref lifetime.ProcessRef, bootID string) error {
	record, err := g.options.Records.RecordLifetimeWorkerContext(ctx, taskdir.LifetimeWorkerFacts{
		PID:          ref.PID(),
		ProcessStart: ref.StartToken(),
		BootID:       bootID,
	})
	if err != nil {
		return err
	}
	if record == nil || record.TaskID != g.options.TaskID || record.PID != ref.PID() || record.ProcessStart != ref.StartToken() {
		return fmt.Errorf("%w: worker writer returned invalid record", errLifetimeGuardianInvalid)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("marshal lifetime worker record: %w", err)
	}
	g.workerPID = record.PID
	g.workerSHA256 = task.ComputeSHA256(data)
	return nil
}

// recordWorkerWait persists the managed-worker completion disposition. The
// candidate is frozen on first observation so retries cannot rewrite its
// timestamp or classification. A worker that never started has no process
// identity; a started worker requires its exact child Wait observation.
func (g *Guardian) recordWorkerWait(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.workerStarted && (!g.workerWaitObserved || g.workerPID <= 0) {
		return fmt.Errorf("%w: exact worker Wait is unavailable", errLifetimeGuardianInvalid)
	}
	g.freezeWorkerWaitFacts()
	record, err := g.options.Records.RecordLifetimeWorkerWaitContext(ctx, *g.workerWaitFacts)
	if err != nil {
		return fmt.Errorf("record lifetime worker Wait: %w", err)
	}
	if !g.workerWaitRecordMatches(record) {
		return fmt.Errorf("%w: worker Wait writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.workerWaitRecorded = true
	return nil
}

// persistWorkerWait retains the lifetime owner and scope until the frozen,
// exact child Wait becomes durable. Once Wait has been consumed there is no
// second process owner that can reconstruct it, so a transient storage failure
// must delay release rather than turn queue state into substitute evidence.
func (g *Guardian) persistWorkerWait(ctx context.Context) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := g.recordWorkerWait(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.WaitRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *Guardian) freezeWorkerWaitFacts() {
	if g.workerWaitFacts != nil {
		return
	}
	if !g.workerStarted {
		g.workerWaitFacts = &taskdir.LifetimeWorkerWaitFacts{
			Result:     task.LifetimeWorkerWaitNotStarted,
			ObservedAt: g.options.Now().UTC(),
		}
		return
	}
	result, exitCode := classifyLifetimeWorkerWait(g.options.Worker, g.workerWaitErr)
	g.workerWaitFacts = &taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: g.workerSHA256,
		ChildPID:     g.workerPID,
		Result:       result,
		ExitCode:     exitCode,
		ObservedAt:   g.options.Now().UTC(),
	}
}

func (g *Guardian) workerWaitRecordMatches(record *task.LifetimeWorkerWaitRecord) bool {
	return record != nil && task.ValidateLifetimeWorkerWaitRecord(record) == nil &&
		record.TaskID == g.options.TaskID && record.ChildPID == g.workerWaitFacts.ChildPID &&
		record.WorkerSHA256 == g.workerWaitFacts.WorkerSHA256 && record.Result == g.workerWaitFacts.Result &&
		sameOptionalExitCode(record.ExitCode, g.workerWaitFacts.ExitCode) &&
		record.ObservedAt == g.workerWaitFacts.ObservedAt.Format(time.RFC3339Nano)
}

func classifyLifetimeWorkerWait(cmd *exec.Cmd, waitErr error) (task.LifetimeWorkerWaitResult, *int) {
	if waitErr == nil {
		exitCode := 0
		return task.LifetimeWorkerWaitSuccess, &exitCode
	}
	processState := cmd.ProcessState
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if exitErr != nil && exitErr.ProcessState != nil {
			processState = exitErr.ProcessState
		}
		if processState != nil {
			exitCode := processState.ExitCode()
			return task.LifetimeWorkerWaitExitError, &exitCode
		}
	}
	if processState != nil {
		exitCode := processState.ExitCode()
		return task.LifetimeWorkerWaitError, &exitCode
	}
	return task.LifetimeWorkerWaitError, nil
}

func sameOptionalExitCode(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (g *Guardian) tryObserveWorkerWait() (bool, error) {
	if g.workerWaitObserved {
		return true, g.workerWaitErr
	}
	if g.workerWait == nil {
		return false, nil
	}
	select {
	case err := <-g.workerWait:
		g.workerWaitObserved = true
		g.workerWaitErr = err
		return true, err
	default:
		return false, nil
	}
}

func (g *Guardian) awaitWorkerWait(ctx context.Context) (bool, error) {
	if observed, err := g.tryObserveWorkerWait(); observed {
		return true, err
	}
	if g.workerWait == nil {
		return false, errors.New("task lifetime guardian: worker wait is unavailable")
	}
	select {
	case err := <-g.workerWait:
		g.workerWaitObserved = true
		g.workerWaitErr = err
		return true, err
	case <-ctx.Done():
		return false, fmt.Errorf("wait for lifetime worker exit: %w", ctx.Err())
	}
}
