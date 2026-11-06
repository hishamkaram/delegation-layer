package app

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type infrastructurePreparationResult struct {
	disposition infrastructure.Disposition
	report      infrastructure.ManualMigrationReport
	err         error
}

// This per-operation worker owns the entire canonical action, including its
// internal partial clients. Result is immutable after Done. Callback expiry
// ends observation only; the fixed resident joins cleanup before disposing of
// its own guards. Options and canonical client-close behavior are unchanged.
type infrastructurePreparationWorker struct {
	done   chan struct{}
	result infrastructurePreparationResult
}

func runInfrastructurePreparation(ctx context.Context, selected infrastructure.LaunchContext, prepare func(infrastructure.LaunchContext) infrastructurePreparationResult) infrastructurePreparationResult {
	if ctx == nil || prepare == nil {
		return infrastructurePreparationResult{err: os.ErrInvalid}
	}
	if err := ctx.Err(); err != nil {
		return infrastructurePreparationResult{err: err}
	}
	accepted := selected.Clone()
	worker := &infrastructurePreparationWorker{done: make(chan struct{})}
	go func() {
		result := infrastructurePreparationResult{err: infrastructure.ErrIdentity}
		defer func() { worker.result = result; close(worker.done) }()
		result = prepare(accepted)
	}()
	select {
	case <-worker.done:
		return worker.result
	case <-ctx.Done():
		// Prefer an already completed action over an expired observation.
		select {
		case <-worker.done:
			return worker.result
		default:
			var retained infrastructure.ResidentCleanupCompletion = &infrastructurePreparationWorkerError{worker: worker, cause: errors.Join(pueue.ErrInFlight, ctx.Err())}
			return infrastructurePreparationResult{err: retained}
		}
	}
}

type infrastructurePreparationWorkerError struct {
	worker *infrastructurePreparationWorker
	cause  error
}

func (e *infrastructurePreparationWorkerError) Error() string {
	return "resident preparation completion retained: " + e.cause.Error()
}

func (e *infrastructurePreparationWorkerError) Unwrap() error         { return e.cause }
func (*infrastructurePreparationWorkerError) RetainsResidentCleanup() {}
func (e *infrastructurePreparationWorkerError) CleanupDone() <-chan struct{} {
	return e.worker.done
}

func (e *infrastructurePreparationWorkerError) CleanupErr() error {
	select {
	case <-e.worker.done:
		return e.worker.result.err
	default:
		return pueue.ErrInFlight
	}
}

func (e *infrastructurePreparationWorkerError) CleanupPending() bool {
	select {
	case <-e.worker.done:
		return infrastructurePreparationRetainsResources(e.worker.result.err)
	default:
		return true
	}
}

func infrastructurePreparationRetainsResources(cause error) bool {
	return task.AnyErrorNode(cause, func(node error) bool {
		if _, retained := node.(interface{ RetainsResidentCleanup() }); !retained {
			return false
		}
		if owner, tracked := node.(interface{ CleanupPending() bool }); tracked {
			return owner.CleanupPending()
		}
		return true
	})
}

func (h infrastructureEntryHost) EnsureOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	result := runInfrastructurePreparation(ctx, selected, func(accepted infrastructure.LaunchContext) infrastructurePreparationResult {
		disposition, err := h.ensureOwnRoot(ctx, operation, accepted, guards, wait)
		return infrastructurePreparationResult{disposition: disposition, err: err}
	})
	return result.disposition, result.err
}

func (h infrastructureEntryHost) PrepareContinuation(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	result := runInfrastructurePreparation(ctx, selected, func(accepted infrastructure.LaunchContext) infrastructurePreparationResult {
		disposition, err := h.prepareContinuation(ctx, operation, accepted, source, guards, wait)
		return infrastructurePreparationResult{disposition: disposition, err: err}
	})
	return result.disposition, result.err
}

func (h infrastructureEntryHost) RecoverSaved(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (infrastructure.Disposition, error) {
	result := runInfrastructurePreparation(ctx, selected, func(accepted infrastructure.LaunchContext) infrastructurePreparationResult {
		disposition, err := h.recoverSaved(ctx, operation, accepted, source, guards, wait)
		return infrastructurePreparationResult{disposition: disposition, err: err}
	})
	return result.disposition, result.err
}

func (h infrastructureEntryHost) MigrateOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	result := runInfrastructurePreparation(ctx, selected, func(accepted infrastructure.LaunchContext) infrastructurePreparationResult {
		disposition, report, err := h.migrateOwnRoot(ctx, operation, accepted, guards, wait)
		return infrastructurePreparationResult{disposition: disposition, report: report, err: err}
	})
	return result.disposition, result.report, result.err
}
