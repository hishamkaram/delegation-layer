package inspection

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	inspectionjournal "github.com/hishamkaram/delegation-layer/internal/inspection/internal/journal"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type (
	Operation        = inspectionjournal.Operation
	GroupJournal     = inspectionjournal.GroupJournal
	GroupPermit      = inspectionjournal.GroupPermit
	StopPermit       = inspectionjournal.StopPermit
	SubmissionPermit = inspectionjournal.SubmissionPermit
	StartPermit      = inspectionjournal.StartPermit
)

func ProbeGroupContext(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, snapshot pueue.QueueSnapshot) (ready bool, resultErr error) {
	return inspectionjournal.ProbeGroupContext(ctx, store, binding, snapshot)
}

func LoadOperation(store *taskdir.Store, taskID string) (operation *Operation, resultErr error) {
	return inspectionjournal.LoadOperation(store, taskID)
}

func LoadOperationContext(ctx context.Context, store *taskdir.Store, taskID string) (operation *Operation, resultErr error) {
	return inspectionjournal.LoadOperationContext(ctx, store, taskID)
}

func OpenOperation(store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenOperation(store, request, binding, now)
}

func OpenOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenOperationContext(ctx, store, request, binding, now)
}

func OpenOperationWithIdentityContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, operationID string, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenOperationWithIdentityContext(ctx, store, request, binding, operationID, now)
}

func OpenConcurrentOperation(store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenConcurrentOperation(store, request, binding, now)
}

func OpenConcurrentOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenConcurrentOperationContext(ctx, store, request, binding, now)
}

func OpenConcurrentOperationWithIdentityContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, operationID string, now time.Time) (*Operation, error) {
	return inspectionjournal.OpenConcurrentOperationWithIdentityContext(ctx, store, request, binding, operationID, now)
}

func OpenGroup(store *taskdir.Store, binding task.SupervisorRef) (journal *GroupJournal, resultErr error) {
	return inspectionjournal.OpenGroup(store, binding)
}

func OpenGroupContext(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	return inspectionjournal.OpenGroupContext(ctx, store, binding)
}

func OpenGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	return inspectionjournal.OpenGroupGeneration(ctx, store, binding)
}

func OpenGroupHistorical(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	return inspectionjournal.OpenGroupHistorical(ctx, store, binding)
}

func ReadGroupRequest(ctx context.Context, store *taskdir.Store) (record contract.GroupRequestRecord, resultErr error) {
	return inspectionjournal.ReadGroupRequest(ctx, store)
}

func PrepareGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (resultErr error) {
	return inspectionjournal.PrepareGroupMigration(ctx, store, legacy, successor)
}

func CompleteGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (resultErr error) {
	return inspectionjournal.CompleteGroupMigration(ctx, store, legacy, successor)
}

func ReadGroupHead(ctx context.Context, store *taskdir.Store) (head contract.GroupRequestRecord, migrated bool, resultErr error) {
	return inspectionjournal.ReadGroupHead(ctx, store)
}

func GroupHeadMatchesBinding(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (present, matches bool, resultErr error) {
	return inspectionjournal.GroupHeadMatchesBinding(ctx, store, binding)
}

func GroupMigrationComplete(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (complete bool, resultErr error) {
	return inspectionjournal.GroupMigrationComplete(ctx, store, legacy, successor)
}

func HasCompletedGroupMigration(ctx context.Context, store *taskdir.Store) (complete bool, resultErr error) {
	return inspectionjournal.HasCompletedGroupMigration(ctx, store)
}

func GroupMigrationPending(ctx context.Context, store *taskdir.Store) (bool, error) {
	return inspectionjournal.GroupMigrationPending(ctx, store)
}

func PreparedGroupMigrationSuccessors(ctx context.Context, store *taskdir.Store) (successors []task.SupervisorRef, resultErr error) {
	return inspectionjournal.PreparedGroupMigrationSuccessors(ctx, store)
}

func OpenGroupWithMigration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, check func(context.Context, contract.GroupRequestRecord) error) (*GroupJournal, error) {
	return inspectionjournal.OpenGroupWithMigration(ctx, store, binding, check)
}
