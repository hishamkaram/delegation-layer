package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func (infrastructureEntryHost) RecoverSaved(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, resultErr error) {
	preparation, err := openInfrastructurePreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	ctx = pueue.WithControlLease(ctx, preparation.admission)
	saved, err := resolveInfrastructureSavedBinding(ctx, preparation.store, source)
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	client, recoveryErr := supervisor.NewClient(ctx, preparation.store.Root, saved, preparation.options, true, preparation.lookup, openNativeSupervisorStore)
	if client != nil && recoveryErr == nil {
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, supervisor.ClosePrivateAdmissionTransition(client, recoveryErr)
}

func (infrastructureEntryHost) MigrateReferenced(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, reference infrastructure.MigrationReference, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, result infrastructure.MigrationResult, resultErr error) {
	preparation, err := openInfrastructurePreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, "", err
	}
	ctx = pueue.WithControlLease(ctx, preparation.admission)
	canonical, migrationErr := supervisor.ExecuteReferencedPrivateMigration(ctx, preparation.store, reference, preparation.options)
	if canonical == "" {
		if migrationErr == nil {
			migrationErr = fmt.Errorf("%w: canonical migration returned no result", infrastructure.ErrIdentity)
		}
		return infrastructure.Disposition{}, "", migrationErr
	}
	result, err = infrastructureMigrationResult(canonical)
	if err == nil {
		// Both canonical results include readiness verification. A prior native
		// Start remains dominant in the independent observation's disposition.
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, result, errors.Join(migrationErr, err)
}

func infrastructureMigrationResult(result pueue.PrivateMigrationResult) (infrastructure.MigrationResult, error) {
	switch result {
	case pueue.PrivateMigrationCompleted:
		return infrastructure.MigrationCompleted, nil
	case pueue.PrivateMigrationWarmReuse:
		return infrastructure.MigrationWarmReuse, nil
	default:
		return "", fmt.Errorf("%w: unknown canonical migration result", infrastructure.ErrIdentity)
	}
}
