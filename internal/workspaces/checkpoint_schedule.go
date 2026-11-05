package workspaces

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// BeginOrdinaryCheckpoint authorizes and submits one finite checkpoint helper
// for an ordinary checkout selected by OrdinaryCheckpointSelection. It owns no
// workspace resource or reservation; its outer root admission is acquired or
// borrowed before preparing the operation. The returned
// acknowledgement is authorized with the checkpoint ID once the immutable
// request is durable; ErrPending then reports the scheduled helper, and any
// other error leaves that same request for observation to redrive.
func BeginOrdinaryCheckpoint(ctx context.Context, store *taskdir.Store, deps OperationOptions, selection workspace.CheckpointSelection, budget time.Duration) (ack ActionAcknowledgement, resultErr error) {
	borrowed := deps.RootAdmission
	lease, err := acquireWorkspaceRootAdmission(ctx, store, deps)
	defer func() { resultErr = workspaceRootAdmissionError(resultErr, lease, borrowed) }()
	if err != nil {
		return ack, err
	}
	deps.RootAdmission = lease
	if selection.Kind != workspace.CheckpointOrdinary || selection.CwdIdentity == nil {
		return ack, task.ErrIdentityMismatch
	}
	if pathErr := RejectUnmanagedPath(selection.Cwd); pathErr != nil {
		return ack, pathErr
	}
	if identityErr := VerifyDirectoryIdentity(selection.Cwd, *selection.CwdIdentity); identityErr != nil {
		return ack, identityErr
	}
	control, cancel := workspaceSchedulingControl(ctx, deps)
	defer cancel()
	intentID, err := randomid.NewTaskID()
	if err != nil {
		return ack, err
	}
	subject, err := checkpointIntentSubject("", intentID, selection)
	if err != nil {
		return ack, err
	}
	spec := OperationSpec{Kind: workspace.OperationCheckpoint, Subject: subject, Budget: budget, Checkpoint: &selection}
	request, err := newWorkspaceOperationRequest(control, store, deps, "", spec)
	if err != nil {
		return ack, err
	}
	operation, err := workspace.OpenOperation(control, store, request)
	if err != nil {
		return ack, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	ack = ActionAcknowledgement{authorized: true, operationID: request.OperationID}
	if err = ScheduleExistingOperation(control, store, operation, deps); err != nil {
		return ack, err
	}
	return ack, workspace.ErrPending
}

// BeginManagedCheckpoint authorizes one checkpoint of a managed workspace
// under its use owner, following verification: the durable request is
// reserved, then indexed by the resource, before its helper is enqueued.
func BeginManagedCheckpoint(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, budget time.Duration) (ack ActionAcknowledgement, resultErr error) {
	if err := ReconcileUse(ctx, store, resource, deps); err != nil {
		return ack, err
	}
	intentID, err := randomid.NewTaskID()
	if err != nil {
		return ack, err
	}
	selection := workspace.CheckpointSelection{Kind: workspace.CheckpointManaged}
	subject, err := checkpointIntentSubject(resource.Request().WorkspaceID, intentID, selection)
	if err != nil {
		return ack, err
	}
	authorize := func(scheduling context.Context, operation *workspace.Operation) error {
		ack.operationID = operation.Request().OperationID
		use := workspace.Subject{Kind: workspace.SubjectOperation, ID: ack.operationID, SHA256: operation.Digest()}
		if _, reserveErr := resource.Reserve(scheduling, use); reserveErr != nil {
			return reserveErr
		}
		ack.authorized = true
		return resource.RecordOperation(scheduling, operation.Request())
	}
	spec := OperationSpec{Purpose: CheckpointPurpose(intentID), Kind: workspace.OperationCheckpoint, Subject: subject, Budget: budget, Checkpoint: &selection}
	operation, err := ScheduleOperation(ctx, store, resource, deps, spec, authorize)
	defer func() { resultErr = errors.Join(resultErr, CloseOperation(operation)) }()
	if err != nil {
		return ack, err
	}
	return ack, workspace.ErrPending
}

// BeginCheckpointRemoval removes one checkpoint's owned ref through a finite
// registered attempt after proving the original's canonical closure. An
// unfinished attempt is redriven and a qualified success is returned. With no
// attempt, or after a closed failed one, this explicit call registers a fresh
// finite request against the observed head before submitting it. Its root
// admission precedes journal and runtime preparation. The acknowledgement is authorized with the attempt that owns the
// removal: an existing registered head, the newly registered attempt, or a
// concurrent winner. ErrPending then reports that attempt still running.
func BeginCheckpointRemoval(ctx context.Context, store *taskdir.Store, deps OperationOptions, checkpointID string, budget time.Duration) (ack ActionAcknowledgement, resultErr error) {
	borrowed := deps.RootAdmission
	lease, err := acquireWorkspaceRootAdmission(ctx, store, deps)
	defer func() { resultErr = workspaceRootAdmissionError(resultErr, lease, borrowed) }()
	if err != nil {
		return ack, err
	}
	deps.RootAdmission = lease
	control, cancel := workspaceSchedulingControl(ctx, deps)
	defer cancel()
	original, err := loadCheckpointCreation(control, store, checkpointID)
	if err != nil {
		return ack, err
	}
	defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
	if _, err = QualifyCheckpoint(control, store, original); err != nil {
		return ack, err
	}
	head, err := reconcileCheckpointRemoval(control, store, original, deps)
	if head.count != 0 {
		ack = ActionAcknowledgement{authorized: true, operationID: head.attempt.OperationID}
	}
	if err != nil || (head.receipt != nil && head.receipt.Status != workspace.CheckpointUnavailable) {
		return ack, err
	}
	return registerCheckpointRemoval(control, store, original, deps, head.count, budget)
}

// registerCheckpointRemoval creates a fresh removal request outside the
// original's lock, registers it after expectedHead and submits only that
// registered request. A lost race leaves its own request unreferenced and
// never submitted, and reports the winner's state instead.
func registerCheckpointRemoval(ctx context.Context, store *taskdir.Store, original *workspace.Operation, deps OperationOptions, expectedHead uint64, budget time.Duration) (ack ActionAcknowledgement, resultErr error) {
	spec := OperationSpec{Kind: workspace.OperationCheckpointRemove, Subject: checkpointRemovalSubject(original), Budget: budget}
	request, err := newWorkspaceOperationRequest(ctx, store, deps, "", spec)
	if err != nil {
		return ack, err
	}
	removal, err := workspace.OpenOperation(ctx, store, request)
	if err != nil {
		return ack, err
	}
	defer func() { resultErr = errors.Join(resultErr, removal.Close()) }()
	_, _, err = original.RegisterCheckpointRemoval(ctx, removal, expectedHead)
	if errors.Is(err, workspace.ErrCheckpointRemovalHeadChanged) {
		winner, observeErr := reconcileCheckpointRemoval(ctx, store, original, deps)
		if winner.count != 0 {
			ack = ActionAcknowledgement{authorized: true, operationID: winner.attempt.OperationID}
		}
		if observeErr == nil && winner.receipt != nil && winner.receipt.Status == workspace.CheckpointUnavailable {
			observeErr = &OperationError{reason: winner.receipt.ReasonCode}
		}
		return ack, observeErr
	}
	if err != nil {
		return ack, err
	}
	ack = ActionAcknowledgement{authorized: true, operationID: request.OperationID}
	if err = ScheduleExistingOperation(ctx, store, removal, deps); err != nil {
		return ack, err
	}
	return ack, workspace.ErrPending
}
