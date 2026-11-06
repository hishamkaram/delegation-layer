package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func (infrastructureEntryHost) recoverSaved(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, resultErr error) {
	preparation, err := openInfrastructurePreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	saved, err := resolveInfrastructureSavedBinding(ctx, preparation.store, source)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	client, recoveryErr := supervisor.RecoverPrivateClient(ctx, preparation.store, saved, preparation.options, preparation.lookup)
	if client != nil && recoveryErr == nil {
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, supervisor.ClosePrivateAdmissionTransition(client, recoveryErr)
}
