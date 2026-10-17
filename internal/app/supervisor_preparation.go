package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func preparePrivateTerminalEvidence(ctx context.Context, root string, supervisor task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	return privateCleanupOwner().PrepareTerminalEvidence(ctx, root, supervisor, jobs)
}
