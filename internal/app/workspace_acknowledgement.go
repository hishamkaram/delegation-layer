package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Authorization is derived from its durable owner before reading the view.
// This acknowledgement is not a second journal, and a read error is not a
// negative acknowledgement. OperationID names only a real, linked helper.
type workspaceActionAcknowledgement struct {
	authorized  bool
	operationID string
}

func workspaceCommandActionResponse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, action string, discard bool, ack workspaceActionAcknowledgement, response Response, actionErr error) commandResult {
	if action == "status" {
		return workspaceCommandResponse(ctx, store, resource, ack.operationID, response, actionErr)
	}
	responseContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	var ackErr error
	if !ack.authorized {
		ack, ackErr = acknowledgeWorkspaceAction(responseContext, store, resource, action, discard, ack.operationID)
	}
	description, descriptionErr := describeWorkspace(responseContext, resource)
	// A prior use may have its own helper. It is not the requested action.
	description.OperationID, description.Operation = ack.operationID, nil
	if ack.operationID != "" {
		descriptionErr = errors.Join(descriptionErr, describeWorkspaceOperation(responseContext, store, resource, ack.operationID, &description))
	}
	response.Workspace = &description
	if ackErr != nil {
		return failed(response, errors.Join(actionErr, descriptionErr, ackErr), 1)
	}
	return finishWorkspaceActionResponse(responseContext, response, action, ack, actionErr, descriptionErr)
}

func finishWorkspaceActionResponse(ctx context.Context, response Response, action string, ack workspaceActionAcknowledgement, actionErr, descriptionErr error) commandResult {
	result := finishWorkspaceCommandResponse(ctx, response, actionErr, descriptionErr)
	if result.code == 3 {
		if ack.authorized {
			result.response.Workspace.NextAction = "watch_workspace"
			result.response.Workspace.OperationID = ack.operationID
		} else {
			result.response.Workspace.ReasonCode = "requested_action_not_authorized"
			result.response.Workspace.NextAction = "retry_" + action
			result.response.Workspace.OperationID, result.response.Workspace.Operation = "", nil
		}
	}
	return result
}

func acknowledgeWorkspaceAction(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, action string, discard bool, operationID string) (workspaceActionAcknowledgement, error) {
	switch action {
	case "exec":
		return acknowledgeWorkspaceVerification(ctx, store, resource, operationID)
	case "release":
		authorized, err := workspaceReleaseAcknowledged(ctx, resource, discard)
		return workspaceActionAcknowledgement{authorized: authorized}, err
	case "forget":
		intent, err := resource.ForgetIntent(ctx)
		if workspace.OperationAbsent(err) {
			return workspaceActionAcknowledgement{}, nil
		}
		if err != nil {
			return workspaceActionAcknowledgement{}, err
		}
		if operationID != "" {
			if err = validateWorkspaceForgetHelper(ctx, store, resource, intent, operationID); err != nil {
				return workspaceActionAcknowledgement{}, err
			}
		}
		return workspaceActionAcknowledgement{authorized: true, operationID: operationID}, nil
	default:
		return workspaceActionAcknowledgement{}, task.ErrInvalidPermit
	}
}

func acknowledgeWorkspaceVerification(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string) (ack workspaceActionAcknowledgement, resultErr error) {
	if operationID == "" {
		return ack, nil
	}
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return ack, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().Kind != workspace.OperationVerify || operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return ack, task.ErrIdentityMismatch
	}
	reservation, err := resource.LatestReservation(ctx)
	if workspace.OperationAbsent(err) {
		return ack, nil
	}
	if err != nil {
		return ack, err
	}
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: operationID, SHA256: operation.Digest()}
	if reservation.Subject == subject {
		ack = workspaceActionAcknowledgement{authorized: true, operationID: operationID}
	}
	return ack, nil
}

// A release reservation is authorization even before its helper exists or its
// successful capture is projected into ReleaseRecord. A failed capture without
// explicit discard leaves the checkout usable and must not revive that action.
func workspaceReleaseAcknowledged(ctx context.Context, resource *workspace.Resource, discard bool) (bool, error) {
	if _, err := resource.Release(ctx); err == nil {
		return true, nil
	} else if !workspace.OperationAbsent(err) {
		return false, err
	}
	reservation, err := resource.LatestReservation(ctx)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if reservation.Subject.Kind != workspace.SubjectOperation {
		return false, nil
	}
	action, err := resource.ReleaseAction(ctx, reservation.Subject)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if action.Discard != discard {
		return false, nil
	}
	closed, err := resource.ReservationClosed(ctx, reservation.Sequence)
	if workspace.OperationAbsent(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if closed.CaptureID == "" {
		return false, task.ErrEvidenceFault
	}
	receipt, err := resource.CaptureReceipt(ctx, closed.CaptureID)
	if err != nil {
		return false, err
	}
	if receipt.Subject != reservation.Subject {
		return false, task.ErrIdentityMismatch
	}
	return receipt.Status == "captured" || action.Discard, nil
}

func validateWorkspaceForgetHelper(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, intent workspace.ForgetIntent, operationID string) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().Subject.ID != intent.OperationID {
		return task.ErrIdentityMismatch
	}
	return validateWorkspaceActionRequest(ctx, resource, operation.Request())
}

func workspacePendingError(err error) bool {
	if !errors.Is(err, workspace.ErrPending) && !errors.Is(err, workspace.ErrBusy) {
		return false
	}
	return isPureErrorSet(err, func(cause error) bool {
		return errors.Is(cause, workspace.ErrPending) || errors.Is(cause, workspace.ErrBusy) ||
			errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, task.ErrLockBusy) ||
			errors.Is(cause, pueue.ErrInFlight) || errors.Is(cause, pueue.ErrSubmissionUncertain) || errors.Is(cause, pueue.ErrEnqueueUncertain)
	})
}
