package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// use selects one reservation's evidence. A task selection also pins the
// task subject that its obligation, baseline and receipt must name.
type use struct {
	reservation workspace.Reservation
	subject     *workspace.Subject
}

// TaskSubject is the canonical capture subject for one immutable task request.
func TaskSubject(request task.TaskRecord) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(request)
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectTask, ID: request.TaskID, SHA256: task.ComputeSHA256(data)}, nil
}

// taskUse resolves the exact reservation bound by task.json and proves that
// it is this task's admission, irrespective of later uses of the resource.
func taskUse(ctx context.Context, resource *workspace.Resource, request task.TaskRecord) (use, error) {
	binding := request.Workspace
	if binding == nil || binding.ID != resource.Request().WorkspaceID {
		return use{}, task.ErrIdentityMismatch
	}
	reservation, err := resource.ReservationAt(ctx, binding.Reservation)
	if workspace.OperationAbsent(err) {
		return use{}, errors.Join(task.ErrEvidenceFault, err)
	}
	if err != nil {
		return use{}, err
	}
	admission := workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: binding.RequestSHA256}
	if reservation.Subject != admission {
		return use{}, task.ErrIdentityMismatch
	}
	subject, err := TaskSubject(request)
	if err != nil {
		return use{}, err
	}
	return use{reservation: reservation, subject: &subject}, nil
}
