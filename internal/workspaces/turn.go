package workspaces

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// TurnUse resolves the reservation bound by task.json for a prelaunch
// baseline. The journal additionally requires that use to be open with this
// task's capture obligation before it accepts the baseline.
func TurnUse(ctx context.Context, resource *workspace.Resource, request task.TaskRecord) (workspace.Reservation, workspace.Subject, error) {
	selected, err := taskUse(ctx, resource, request)
	if err != nil {
		return workspace.Reservation{}, workspace.Subject{}, err
	}
	return selected.reservation, *selected.subject, nil
}

// CaptureTurnBaseline resolves the baseline for one capture operation. Only a
// task capture elected for the open use's capture purpose has a turn; other
// subjects report applies=false. A nil baseline means none was recorded.
func CaptureTurnBaseline(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest) (applies bool, baseline *workspace.TurnBaseline, resultErr error) {
	if request.Kind != workspace.OperationCapture || request.Subject.Kind != workspace.SubjectTask {
		return false, nil, nil
	}
	reservation, err := resource.Reservation(ctx)
	if err != nil {
		return true, nil, err
	}
	obligation, err := resource.CaptureObligation(ctx, reservation.Sequence)
	if err != nil {
		return true, nil, err
	}
	reference, err := resource.AssociatedOperation(ctx, workspace.CapturePurpose(reservation.Sequence))
	if err != nil {
		return true, nil, err
	}
	if obligation.Subject != request.Subject || reference.OperationID != request.OperationID {
		return true, nil, task.ErrIdentityMismatch
	}
	recorded, err := resource.TurnBaseline(ctx, reservation.Sequence)
	if workspace.OperationAbsent(err) {
		return true, nil, nil
	}
	if err != nil {
		return true, nil, err
	}
	if recorded.Subject != request.Subject {
		return true, nil, task.ErrIdentityMismatch
	}
	return true, &recorded, nil
}

// ManagedBaselineRef names the owned tree ref for one turn baseline.
func ManagedBaselineRef(rootID, workspaceID string, sequence uint64) string {
	return fmt.Sprintf("refs/delegate/%s/workspaces/%s/baselines/%020d", rootID, workspaceID, sequence)
}
