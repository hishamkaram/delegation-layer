package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func AcknowledgeAction(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, action string, discard bool, operationID string) (ActionAcknowledgement, error) {
	switch action {
	case "exec":
		return acknowledgeWorkspaceVerification(ctx, store, resource, operationID)
	case "release":
		authorized, err := ReleaseAcknowledged(ctx, resource, discard)
		return ActionAcknowledgement{authorized: authorized}, err
	case "forget":
		intent, err := resource.ForgetIntent(ctx)
		if workspace.OperationAbsent(err) {
			return ActionAcknowledgement{}, nil
		}
		if err != nil {
			return ActionAcknowledgement{}, err
		}
		if operationID != "" {
			if err = validateWorkspaceForgetHelper(ctx, store, resource, intent, operationID); err != nil {
				return ActionAcknowledgement{}, err
			}
		}
		return ActionAcknowledgement{authorized: true, operationID: operationID}, nil
	default:
		return ActionAcknowledgement{}, task.ErrInvalidPermit
	}
}

func acknowledgeWorkspaceVerification(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string) (ack ActionAcknowledgement, resultErr error) {
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
		ack = ActionAcknowledgement{authorized: true, operationID: operationID}
	}
	return ack, nil
}

// A release reservation is authorization even before its helper exists or its
// successful capture is projected into ReleaseRecord. A failed capture without
// explicit discard leaves the checkout usable and must not revive that action.
func ReleaseAcknowledged(ctx context.Context, resource *workspace.Resource, discard bool) (bool, error) {
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
	return ValidateActionRequest(ctx, resource, operation.Request())
}
