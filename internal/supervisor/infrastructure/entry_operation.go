package infrastructure

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Birth reads use the same lifecycle decision as the serialized assumption and
// publication commits. This early check is not their mutation authority: both
// commits revalidate under the actual inherited instance lease and transaction.
func readEntryOperation(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, intent StartupIntent) (Operation, error) {
	reader := passiveLifecycleReader{passiveOwnerReader{passiveIntentReader{store}}}
	if err := requireLifecycleStartup(ctx, reader, pin, intent); err != nil {
		return Operation{}, err
	}
	reference := intent.Inputs.Context
	var body epochLifecycleIntent
	if intent.Inputs.Kind == StartupUpgrade {
		snapshot, err := readReferencedLifecycle(ctx, reader, pin, epochLifecycleReference{ID: intent.Inputs.UpgradeID, SHA256: intent.Inputs.UpgradeSHA256})
		if err != nil {
			return Operation{}, err
		}
		body, reference = snapshot.intent, snapshot.intent.SuccessorContext
	}
	selected, err := readLaunchContext(ctx, store, reference)
	if err != nil {
		return Operation{}, err
	}
	if intent.Inputs.Kind == StartupUpgrade {
		return newUpgradeOperation(intent, selected, body)
	}
	return newRootOwnerOperation(intent, selected)
}
