package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

var errWorkspaceCaptureUnavailable = errors.New("workspace capture is unavailable; checkout retained")

type workspaceActionOperationState uint8

const (
	workspaceActionClosedFailure workspaceActionOperationState = iota + 1
	workspaceActionClosedSuccess
	workspaceActionUnfinished
)

func beginWorkspaceRelease(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, discard bool, budget time.Duration) (ack workspaceActionAcknowledgement, resultErr error) {
	if _, err := resource.Release(ctx); err == nil {
		ack.authorized = true
		return ack, reconcileWorkspaceRemoval(ctx, store, resource, deps, true)
	} else if !workspace.OperationAbsent(err) {
		return ack, err
	}
	if err := reconcileWorkspaceUse(ctx, store, resource, deps); err != nil {
		return ack, err
	}
	// Reconciliation may have completed a release capture whose reservation
	// closed before ReleaseRecord publication. Do not authorize another action.
	if authorized, err := workspaceReleaseAcknowledged(ctx, resource, discard); err != nil {
		return ack, err
	} else if authorized {
		ack.authorized = true
		return ack, reconcileWorkspaceRemoval(ctx, store, resource, deps, true)
	}
	id, err := task.NewTaskID()
	if err != nil {
		return ack, err
	}
	subject, err := resource.RecordReleaseAction(ctx, workspace.ReleaseAction{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: id, Discard: discard, BudgetNanos: int64(budget)})
	if err != nil {
		return ack, err
	}
	reservation, err := resource.Reserve(ctx, subject)
	if err != nil {
		return ack, err
	}
	ack.authorized = true
	if err = resource.RecordReleaseCapture(ctx, reservation, subject); err != nil {
		return ack, err
	}
	return ack, reconcileWorkspaceUse(ctx, store, resource, deps)
}

func completeWorkspaceReleaseCapture(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, receipt workspace.CaptureReceipt) error {
	action, err := resource.ReleaseAction(ctx, receipt.Subject)
	if err != nil {
		return err
	}
	if receipt.Status != "captured" && !action.Discard {
		return errWorkspaceCaptureUnavailable
	}
	if err = resource.RecordRelease(ctx, workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: action.WorkspaceID, OperationID: action.ID, Discard: action.Discard}); err != nil {
		return err
	}
	return reconcileWorkspaceRemoval(ctx, store, resource, deps, false)
}

func reconcileClosedReleaseCapture(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, sequence uint64) error {
	obligation, err := resource.CaptureObligation(ctx, sequence)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if obligation.Predecessor != nil {
		return nil
	}
	closed, err := resource.ReservationClosed(ctx, sequence)
	if err != nil {
		return err
	}
	if closed.CaptureID == "" {
		return nil
	}
	receipt, err := resource.CaptureReceipt(ctx, closed.CaptureID)
	if err != nil {
		return err
	}
	action, err := resource.ReleaseAction(ctx, obligation.Subject)
	if err != nil {
		return err
	}
	if receipt.Status != "captured" && !action.Discard {
		return nil
	}
	return completeWorkspaceReleaseCapture(ctx, store, resource, deps, receipt)
}

func reconcileWorkspaceRemoval(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, retry bool) (resultErr error) {
	release, err := resource.Release(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = resource.Removed(ctx); err == nil {
		return nil
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	subject, err := workspaceRemovalSubject(release)
	if err != nil {
		return err
	}
	operation, err := latestWorkspaceOperation(ctx, store, resource, workspace.OperationRemove, subject)
	if err != nil && !workspace.OperationAbsent(err) {
		return err
	}
	if operation != nil {
		completed, operationErr := reconcileExistingWorkspaceRemoval(ctx, resource, store, operation, release, deps, retry)
		if completed {
			return operationErr
		}
	}
	budget, err := workspaceReleaseBudget(ctx, resource, release)
	if err != nil {
		return err
	}
	return submitWorkspaceRemoval(ctx, store, resource, deps, release, budget)
}

func reconcileExistingWorkspaceRemoval(ctx context.Context, resource *workspace.Resource, store *taskdir.Store, operation *workspace.Operation, release workspace.ReleaseRecord, deps Dependencies, retry bool) (completed bool, resultErr error) {
	_, _, completionErr := acceptedWorkspaceOperation(ctx, store, operation)
	if completionErr == nil {
		return true, errors.Join(resource.RecordRemoved(ctx, release.OperationID), operation.Close())
	}
	if workspacePendingError(completionErr) {
		return true, errors.Join(completionErr, scheduleExistingWorkspaceOperation(ctx, store, operation, deps), operation.Close())
	}
	if !retry {
		return true, errors.Join(completionErr, operation.Close())
	}
	if _, err := workspaceOperationClosed(ctx, operation); err != nil {
		return true, errors.Join(err, operation.Close())
	}
	if err := operation.Close(); err != nil {
		return true, err
	}
	return false, nil
}

func workspaceReleaseBudget(ctx context.Context, resource *workspace.Resource, release workspace.ReleaseRecord) (time.Duration, error) {
	action, err := resource.ReadReleaseAction(ctx, release.OperationID)
	if err != nil {
		return 0, err
	}
	if action.Discard != release.Discard {
		return 0, task.ErrIdentityMismatch
	}
	return time.Duration(action.BudgetNanos), nil
}

func submitWorkspaceRemoval(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, release workspace.ReleaseRecord, budget time.Duration) (resultErr error) {
	subject, err := workspaceRemovalSubject(release)
	if err != nil {
		return err
	}
	id, err := task.NewTaskID()
	if err != nil {
		return err
	}
	operation, err := scheduleWorkspaceOperation(ctx, store, resource, deps, workspaceOperationSpec{purpose: "remove-" + id + ".json", kind: workspace.OperationRemove, subject: subject, budget: budget, discard: release.Discard}, nil)
	defer func() { resultErr = errors.Join(resultErr, closeWorkspaceOperation(operation)) }()
	if err != nil {
		return err
	}
	return workspace.ErrPending
}

// Deadlines are not attempt order: a shorter retry can follow a longer failed
// attempt. The unique unfinished helper owns recovery; once all helpers close,
// a successful candidate takes precedence over a closed failed attempt.
func latestWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, kind string, subject workspace.Subject) (latest *workspace.Operation, resultErr error) {
	ids, err := resource.OperationIDs(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil && latest != nil {
			resultErr = errors.Join(resultErr, latest.Close())
			latest = nil
		}
	}()
	var latestRank workspaceActionOperationState
	for _, id := range ids {
		operation, rank, loadErr := readWorkspaceActionOperation(ctx, store, resource, id, kind, subject)
		if loadErr != nil {
			return latest, loadErr
		}
		if operation == nil {
			continue
		}
		latest, latestRank, err = selectWorkspaceActionOperation(latest, operation, latestRank, rank)
		if err != nil {
			return latest, err
		}
	}
	if latest == nil {
		return nil, os.ErrNotExist
	}
	return latest, nil
}

func selectWorkspaceActionOperation(previous, current *workspace.Operation, previousState, currentState workspaceActionOperationState) (*workspace.Operation, workspaceActionOperationState, error) {
	if currentState == workspaceActionUnfinished && previousState == workspaceActionUnfinished {
		return previous, previousState, errors.Join(task.ErrEvidenceFault, current.Close())
	}
	if currentState <= previousState {
		return previous, previousState, current.Close()
	}
	if previous != nil {
		if err := previous.Close(); err != nil {
			return previous, previousState, errors.Join(err, current.Close())
		}
	}
	return current, currentState, nil
}

func readWorkspaceActionOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, id, kind string, subject workspace.Subject) (*workspace.Operation, workspaceActionOperationState, error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return nil, 0, err
	}
	request := operation.Request()
	if request.WorkspaceID != resource.Request().WorkspaceID || (request.Kind == kind && request.Subject != subject) {
		return nil, 0, errors.Join(task.ErrIdentityMismatch, operation.Close())
	}
	if request.Kind != kind {
		return nil, 0, operation.Close()
	}
	rank, err := workspaceActionOperationRank(ctx, operation)
	if err != nil {
		return nil, 0, errors.Join(err, operation.Close())
	}
	return operation, rank, nil
}

func workspaceActionOperationRank(ctx context.Context, operation *workspace.Operation) (workspaceActionOperationState, error) {
	if _, err := workspaceOperationClosed(ctx, operation); err != nil {
		if workspacePendingError(err) {
			return workspaceActionUnfinished, nil
		}
		return 0, err
	}
	result, err := operation.Result(ctx)
	if workspace.OperationAbsent(err) {
		return workspaceActionClosedFailure, nil
	}
	if err != nil {
		return 0, err
	}
	if result.Status == "succeeded" {
		return workspaceActionClosedSuccess, nil
	}
	return workspaceActionClosedFailure, nil
}
