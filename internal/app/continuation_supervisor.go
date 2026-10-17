package app

import (
	"context"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func continuationSupervisorOptions(deps Dependencies) supervisorcore.ContinuationOptions {
	return supervisorcore.ContinuationOptions{
		Options: deps.SupervisorOptions, InitialExecutable: deps.InitialSupervisorExecutable,
		OpenStore: func(ctx context.Context, root string) (*taskdir.Store, error) {
			return openStoreContext(ctx, root, deps.storeDependencies(), false)
		},
	}
}

func rebindContinuationSupervisor(ctx context.Context, root string, saved task.SupervisorRef, deps Dependencies) (task.SupervisorRef, error) {
	return supervisorcore.RebindContinuation(ctx, root, saved, continuationSupervisorOptions(deps))
}

func prepareContinuationSuccessor(ctx context.Context, root string, successor task.SupervisorRef, deps Dependencies) (task.SupervisorRef, error) {
	return supervisorcore.PrepareContinuationSuccessor(ctx, root, successor, continuationSupervisorOptions(deps))
}

func revalidateContinuationSuccessor(ctx context.Context, root string, candidate task.SupervisorRef, deps storeDependencies) (task.SupervisorRef, error) {
	return supervisorcore.RevalidateContinuationSuccessor(ctx, root, candidate, func(ctx context.Context, root string) (*taskdir.Store, error) {
		return openStoreContext(ctx, root, deps, false)
	})
}
