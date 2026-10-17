package app

import (
	"context"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

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
