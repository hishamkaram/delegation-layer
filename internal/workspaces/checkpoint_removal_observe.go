package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// ObserveCheckpointRemoval observes one acknowledged removal attempt, named
// by its operation ID, beside the checkpoint's current metadata. The attempt
// is authenticated against the original journal before any effect: selected
// is nil until then and afterwards always names that attempt, through later
// history, runtime, receipt or close faults. The registered history is
// validated before any effect. A historical attempt is read from its
// committed receipt only. The selected attempt is advanced, through the same
// owner as ObserveCheckpoint, only while it heads the inventory read here; an
// attempt registered later is never advanced by this call. current is the
// fresh validated description, whose Removal is the latest attempt; both
// Attempts count that same inventory. A failed current description is zero.
func ObserveCheckpointRemoval(ctx context.Context, store *taskdir.Store, deps OperationOptions, checkpointID, operationID string) (current CheckpointDescription, selected *CheckpointRemovalDescription, resultErr error) {
	control, cancel := workspaceSchedulingControl(ctx, deps)
	defer cancel()
	original, err := loadCheckpointCreation(control, store, checkpointID)
	if err != nil {
		return current, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
	attempt, err := authenticateCheckpointRemoval(control, store, original, operationID)
	if attempt.OperationID != "" {
		selected = &CheckpointRemovalDescription{OperationID: attempt.OperationID}
	}
	if err != nil {
		return current, selected, err
	}
	attempts, err := checkpointRemovalInventoryWith(control, original, attempt)
	if err != nil {
		return current, selected, err
	}
	var observeErr error
	if attempt.Sequence == uint64(len(attempts)) {
		_, observeErr = advanceCheckpointRemovalHead(control, store, original, deps, attempts)
	}
	observeErr = errors.Join(observeErr, describeCheckpointRemovalState(control, original, attempt, selected))
	if observeErr != nil && !PendingError(observeErr) {
		return current, selected, observeErr
	}
	current, err = describeCheckpoint(control, original)
	if err == nil && current.Removal != nil {
		selected.Attempts = current.Removal.Attempts
	}
	return current, selected, errors.Join(observeErr, err)
}

// authenticateCheckpointRemoval proves operationID names a registered
// removal attempt of original through the canonical registration owner,
// which binds the request's kind, subject and digest to its reference. It
// only reads. A failed load or authentication returns a zero reference; an
// authenticated reference is returned even when closing its request handle
// then fails, joined with that close error.
func authenticateCheckpointRemoval(ctx context.Context, store *taskdir.Store, original *workspace.Operation, operationID string) (workspace.CheckpointRemovalReference, error) {
	removal, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return workspace.CheckpointRemovalReference{}, err
	}
	attempt, err := registeredCheckpointRemoval(ctx, original, removal)
	return attempt, errors.Join(err, removal.Close())
}

// checkpointRemovalInventoryWith reads original's attempts with a validated
// history. The journal only appends, so the authenticated attempt keeps its
// sequence in every later inventory; anything else is an evidence fault.
func checkpointRemovalInventoryWith(ctx context.Context, original *workspace.Operation, attempt workspace.CheckpointRemovalReference) ([]workspace.CheckpointRemovalReference, error) {
	attempts, err := original.CheckpointRemovalAttempts(ctx)
	if err != nil {
		return nil, err
	}
	if attempt.Sequence == 0 || attempt.Sequence > uint64(len(attempts)) || attempts[attempt.Sequence-1] != attempt {
		return nil, task.ErrEvidenceFault
	}
	if err = requireCheckpointRemovalHistory(ctx, original, attempts); err != nil {
		return nil, err
	}
	return attempts, nil
}
