package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func RecordTaskCapture(store *taskdir.Store, request task.TaskRecord) (resultErr error) {
	if request.Workspace == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	data, err := task.MarshalCanonical(request)
	if err != nil {
		return err
	}
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: request.TaskID, SHA256: task.ComputeSHA256(data)}
	previous, previousErr := resource.CaptureObligation(ctx, request.Workspace.Reservation)
	if previousErr == nil {
		if previous.Subject != subject {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	if !workspace.OperationAbsent(previousErr) {
		return previousErr
	}
	reservation, err := resource.Reservation(ctx)
	if err != nil {
		return err
	}
	if reservation.Sequence != request.Workspace.Reservation || reservation.Subject.Kind != workspace.SubjectAdmission || reservation.Subject.ID != request.TaskID || reservation.Subject.SHA256 != request.Workspace.RequestSHA256 {
		return task.ErrIdentityMismatch
	}
	return resource.RecordCaptureObligationWithBudget(ctx, reservation, subject, request.Workspace.BudgetNanos)
}
