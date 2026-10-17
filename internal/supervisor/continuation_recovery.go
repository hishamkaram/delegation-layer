package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// RecoverContinuationSelection reopens the recorded supervisor after static
// continuation admission. The saved reference changes only after both gates.
func RecoverContinuationSelection(ctx context.Context, root string, arguments *AdmissionSelection, options ContinuationOptions) error {
	if arguments == nil || arguments.SavedSupervisor == nil {
		return nil
	}
	rebound, err := RebindContinuation(ctx, root, *arguments.SavedSupervisor, options)
	if err != nil {
		return err
	}
	if pueue.IsPrivateConfig(root, rebound.ConfigPath) {
		rebound, err = PrepareContinuationSuccessor(ctx, root, rebound, options)
		if err != nil {
			return err
		}
	}
	*arguments.SavedSupervisor = rebound
	return nil
}
