package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func preparePrivateTerminalEvidence(ctx context.Context, root string, supervisor task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	return privateCleanupOwner().PrepareTerminalEvidence(ctx, root, supervisor, jobs)
}

func prepareEndedInspectionEvidence(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job) (resultErr error) {
	return privateCleanupOwner().PrepareEndedInspectionEvidence(ctx, store, group, id, job)
}
