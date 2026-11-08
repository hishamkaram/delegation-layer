package infrastructurehost

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// The predecessor metadata owns the original binding. Canonical generation
// resolution owns its successor; this fixed host borrows resident authority
// while preparing that successor and never rewrites the predecessor.
func (h Host) prepareContinuation(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, resultErr error) {
	preparation, err := h.openPreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	original, err := supervisor.ReadTaskMetadataSupervisor(ctx, preparation.store, source)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	options := supervisor.ContinuationOptions{
		Options: preparation.options, InitialExecutable: preparation.lookup.InitialExecutable,
		Environment: preparation.lookup.Environment, OpenStore: h.stores.OpenNative,
	}
	successor, err := supervisor.RebindContinuation(ctx, preparation.store.Root, original, options)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	successor, err = supervisor.PrepareContinuationSuccessor(ctx, preparation.store.Root, successor, options)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	client, preparationErr := supervisor.RecoverPrivateClient(ctx, preparation.store, successor, preparation.options, preparation.lookup)
	if client != nil && preparationErr == nil {
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, supervisor.ClosePrivateAdmissionTransition(client, preparationErr)
}
