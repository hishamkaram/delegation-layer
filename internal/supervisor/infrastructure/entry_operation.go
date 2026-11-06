package infrastructure

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The immutable startup intent owns artifact and context selection. Assumption
// and publication independently revalidate the actual instance lease and root.
func readEntryOperation(ctx context.Context, store *taskdir.ReadOnlyStore, intent StartupIntent) (Operation, error) {
	if err := intent.Validate(); err != nil {
		return Operation{}, err
	}
	selected, err := readLaunchContext(ctx, store, intent.Inputs.Context)
	if err != nil {
		return Operation{}, err
	}
	return newRootOwnerOperation(intent, selected)
}
