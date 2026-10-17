package app

import (
	"context"
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func reconcileSavedSupervisor(ctx context.Context, root string, options pueue.Options) (resultErr error) {
	return supervisorcore.ReconcileSaved(ctx, root, options, openNativeSupervisorStore)
}

func prepareInspectionMigrationSupervisorWithLock(ctx context.Context, store *taskdir.Store, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	pending, err := pueue.PrivateSupervisorMigrationPending(store.Root)
	if err != nil {
		return nil, err
	}
	if !pending {
		return prepareCurrentPrivateSupervisor(ctx, store.Root, deps, options)
	}
	proposals, err := inspection.PreparedGroupMigrationSuccessors(ctx, store)
	if err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return prepareCurrentPrivateSupervisor(ctx, store.Root, deps, options)
	}
	recorded, err := selectPreparedPrivateMigrationTarget(store.Root, proposals)
	if err != nil {
		return nil, err
	}
	if err = supervisorcore.ValidateSavedStateSupervisorPair(store.Root, recorded); err != nil {
		return retargetUnavailableInspectionSuccessor(ctx, store, deps, options, err)
	}
	client, err := pueue.PrepareRecordedPrivateMigrationWithLock(ctx, store.Root, recorded, options)
	if err != nil {
		return retargetUnavailableInspectionSuccessor(ctx, store, deps, options, err)
	}
	return client, nil
}

func retargetUnavailableInspectionSuccessor(ctx context.Context, store *taskdir.Store, deps Dependencies, options pueue.Options, cause error) (*pueue.Client, error) {
	if errors.Is(cause, pueue.ErrInFlight) || ctx.Err() != nil {
		return nil, cause
	}
	allowed, err := pueue.PrivateMigrationRetargetableWithLock(store.Root)
	if err != nil || !allowed {
		return nil, &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: errors.Join(cause, err, pueue.ErrPrivateSupervisorMigrationRequired)}
	}
	current, err := prepareCurrentPrivateSupervisor(ctx, store.Root, deps, options)
	if err != nil {
		return nil, errors.Join(cause, err)
	}
	head, _, err := inspection.ReadGroupHead(ctx, store)
	if err == nil {
		err = inspection.PrepareGroupMigration(ctx, store, head, supervisorcore.InspectionGroup(store.RootID, current.Binding()))
	}
	if err != nil {
		return nil, errors.Join(cause, err, current.CloseAfterPending(err))
	}
	return current, nil
}

func selectPreparedPrivateMigrationTarget(root string, proposals []task.SupervisorRef) (task.SupervisorRef, error) {
	var selected task.SupervisorRef
	found := false
	for _, proposal := range proposals {
		matches, err := pueue.PrivateMigrationTargetsWithLock(root, proposal)
		if err != nil {
			return task.SupervisorRef{}, err
		}
		if matches {
			if found {
				return task.SupervisorRef{}, &pueue.PrivateRecoveryError{ReasonCode: "identity_conflict", Err: pueue.ErrPrivateSupervisorMigrationRequired}
			}
			selected, found = proposal, true
		}
	}
	if !found {
		return task.SupervisorRef{}, &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: pueue.ErrPrivateSupervisorMigrationRequired}
	}
	return selected, nil
}
