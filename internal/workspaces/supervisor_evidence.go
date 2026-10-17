package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// inspectPrivateWorkspaceOperation is the application's auxiliary-owner
// dispatch. The queue transports one label/receipt; the canonical journal
// family alone decides closure and whether that exact row can be removed.
func InspectPrivateOperation(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job) (found bool, resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err := rejectInspectionOperationCollision(ctx, store, id); err != nil {
		return true, err
	}
	if err := validatePrivateWorkspaceIdentity(ctx, store, group, id, job, operation); err != nil {
		return true, err
	}
	return true, ReconcileEndedWorkspaceOperation(ctx, operation, job)
}

func rejectInspectionOperationCollision(ctx context.Context, store *taskdir.Store, id string) error {
	other, err := inspection.LoadOperationContext(ctx, store, id)
	if err == nil {
		return errors.Join(task.ErrIdentityMismatch, other.Close())
	}
	if !task.RecordAbsent(err) {
		return err
	}
	return nil
}

func validatePrivateWorkspaceIdentity(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job, operation *workspace.Operation) error {
	request := operation.Request()
	receipt, err := operation.Receipt(ctx)
	if err != nil {
		return err
	}
	matches, err := supervisorcore.PrivateMigrationSupervisorMatches(store.Root, group.Supervisor, request.Supervisor)
	if err != nil {
		return err
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: id, QueueGroup: "default"}
	if !matches || receipt.NumericTaskID != job.ID || job.Label == nil || *job.Label != identity.Label() || job.Group != identity.Group() {
		return task.ErrIdentityMismatch
	}
	return nil
}

func ReconcileEndedWorkspaceOperation(ctx context.Context, operation *workspace.Operation, _ pueue.Job) error {
	_, err := ReadOperationProof(ctx, operation)
	return err
}
