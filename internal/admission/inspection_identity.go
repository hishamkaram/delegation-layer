package admission

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func InspectionIdentityContext(ctx context.Context, operation *inspection.Operation) (pueue.InspectionIdentity, error) {
	record := operation.Request()
	identity := record.SupervisorIdentity()
	receipt, err := operation.ReceiptContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return identity, nil
	}
	if err != nil {
		return identity, err
	}
	identity.NumericTaskID = receipt.NumericTaskID
	return identity, nil
}

// The resource reservation is the only admission-attempt authority. Every
// logical-task consumer resolves it here; queue workers already carry the
// resolved identity and load their exact journal directly.
func OperationID(ctx context.Context, store *taskdir.Store, request task.TaskRecord) (identity string, resultErr error) {
	if store == nil || request.RootID != store.RootID {
		return "", task.ErrIdentityMismatch
	}
	if request.Workspace == nil {
		return request.TaskID, task.ValidateTaskID(request.TaskID)
	}
	binding := request.Workspace
	resource, err := workspace.LoadResource(ctx, store, binding.ID)
	if err != nil {
		return "", errors.Join(task.ErrEvidenceFault, err)
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	attempt, err := resource.ResolveAdmissionAttempt(ctx, binding.Reservation, workspace.Subject{
		Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: binding.RequestSHA256,
	})
	if err != nil {
		return "", err
	}
	return attempt.InspectionOperationID()
}

func LoadInspectionOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord) (*inspection.Operation, error) {
	id, err := OperationID(ctx, store, request)
	if err != nil {
		return nil, err
	}
	return inspection.LoadOperationContext(ctx, store, id)
}
