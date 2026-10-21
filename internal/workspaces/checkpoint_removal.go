package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// checkpointRemovalHead is the reconciled state of a checkpoint's latest
// registered removal attempt. receipt is nil without any attempt or while the
// head is unfinished.
type checkpointRemovalHead struct {
	count   uint64
	attempt workspace.CheckpointRemovalReference
	receipt *workspace.CheckpointRemovalReceipt
}

// reconcileCheckpointRemoval advances only the registered head attempt of a
// freshly read inventory. It never registers another attempt. The returned
// head names the attempt even when the history is refused, so an action keeps
// its acknowledgement with the fault.
func reconcileCheckpointRemoval(ctx context.Context, store *taskdir.Store, original *workspace.Operation, deps OperationOptions) (head checkpointRemovalHead, resultErr error) {
	attempts, err := original.CheckpointRemovalAttempts(ctx)
	if err != nil || len(attempts) == 0 {
		return head, err
	}
	head.count, head.attempt = uint64(len(attempts)), attempts[len(attempts)-1]
	if err = requireCheckpointRemovalHistory(ctx, original, attempts); err != nil {
		return head, err
	}
	head.receipt, err = advanceCheckpointRemovalHead(ctx, store, original, deps, attempts)
	return head, err
}

// advanceCheckpointRemovalHead advances the fixed head of attempts, an
// inventory whose history the caller has validated: an unfinished head is
// redriven with its original request, and a closed head receives its
// qualified receipt. Every registered request is authenticated before the
// head is ranked, because ranking may fence or reconcile its closure. An
// attempt registered after attempts was read is never advanced here.
func advanceCheckpointRemovalHead(ctx context.Context, store *taskdir.Store, original *workspace.Operation, deps OperationOptions, attempts []workspace.CheckpointRemovalReference) (receipt *workspace.CheckpointRemovalReceipt, resultErr error) {
	removal, err := openCheckpointRemovalHead(ctx, store, original, attempts)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, removal.Close()) }()
	state, err := workspaceActionOperationRank(ctx, removal)
	if err != nil {
		return nil, err
	}
	if state == workspaceActionUnfinished {
		return nil, errors.Join(workspace.ErrPending, ScheduleExistingOperation(ctx, store, removal, deps))
	}
	qualified, err := qualifyCheckpointRemoval(ctx, store, original, removal, attempts[len(attempts)-1])
	if err != nil {
		return nil, err
	}
	return &qualified, nil
}

// requireCheckpointRemovalHistory enforces the registration invariant over
// one read inventory: an attempt registers only after its predecessor closed
// without success, so every attempt before the head holds a stored
// unavailable receipt. The head may hold any valid receipt or none. A missing
// or successful earlier receipt is an evidence fault; every other error is
// returned whole. It reads only the original's records.
func requireCheckpointRemovalHistory(ctx context.Context, original *workspace.Operation, attempts []workspace.CheckpointRemovalReference) error {
	for index := 0; index+1 < len(attempts); index++ {
		receipt, err := original.CheckpointRemovalReceipt(ctx, attempts[index])
		if err != nil {
			return requireCheckpointRemovalRecord(err)
		}
		if receipt.Status != workspace.CheckpointUnavailable {
			return task.ErrEvidenceFault
		}
	}
	return nil
}

// openCheckpointRemovalHead authenticates every registered attempt's request
// and retains only the head's handle. Each predecessor handle is closed before
// the head is opened, so any authentication or close fault leaves no head to
// rank.
func openCheckpointRemovalHead(ctx context.Context, store *taskdir.Store, original *workspace.Operation, attempts []workspace.CheckpointRemovalReference) (*workspace.Operation, error) {
	last := len(attempts) - 1
	for _, attempt := range attempts[:last] {
		predecessor, err := loadCheckpointRemovalAttempt(ctx, store, original, attempt)
		if err != nil {
			return nil, err
		}
		if err = predecessor.Close(); err != nil {
			return nil, err
		}
	}
	return loadCheckpointRemovalAttempt(ctx, store, original, attempts[last])
}

// loadCheckpointRemovalAttempt opens the exact removal request a registered
// reference names and binds it to the original checkpoint.
func loadCheckpointRemovalAttempt(ctx context.Context, store *taskdir.Store, original *workspace.Operation, attempt workspace.CheckpointRemovalReference) (*workspace.Operation, error) {
	removal, err := workspace.LoadOperation(ctx, store, attempt.OperationID)
	if err != nil {
		return nil, requireCheckpointRemovalRecord(err)
	}
	request := removal.Request()
	if request.Kind != workspace.OperationCheckpointRemove || request.Subject != checkpointRemovalSubject(original) || removal.Digest() != attempt.RequestSHA256 {
		return nil, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch, removal.Close())
	}
	return removal, nil
}

// requireCheckpointRemovalRecord classifies a missing required record as an
// evidence fault and keeps every other error unchanged.
func requireCheckpointRemovalRecord(err error) error {
	if workspace.OperationAbsent(err) {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return err
}
