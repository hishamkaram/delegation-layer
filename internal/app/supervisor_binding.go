package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// openNativeSupervisorStore retains the native interpreter composition used
// by saved-endpoint recovery, independent of a command's preparation catalog.
func openNativeSupervisorStore(ctx context.Context, root string) (*taskdir.Store, error) {
	return openStoreContext(ctx, root, Dependencies{}.storeDependencies(), false)
}
