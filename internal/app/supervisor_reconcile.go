package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// reconcilePublishedSupervisorWithLock is the single application eligibility
// gate for path-only recovery. Inventory cannot select the active generation;
// the retained pair is derived from the published content identity.
func reconcilePublishedSupervisorWithLock(ctx context.Context, store *taskdir.Store, options pueue.Options, explicitMigration bool) error {
	// A healthy published pair does not depend on staged or retained inventory.
	// Reuse its canonical validator before considering any relocation candidate.
	ready, err := verifyAvailablePublishedSupervisorPair(ctx, store.Root, options)
	if err != nil || ready {
		return err
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		return err
	}
	settled, err := inspectionAllowsPairReconciliation(ctx, store, published)
	if err != nil || !settled {
		return err
	}
	client, daemon, available, err := resolveReconciliationPair(store.Root, published, explicitMigration)
	if err != nil || !available {
		return err
	}
	err = pueue.ReconcilePublishedPrivatePairWithLock(ctx, store.Root, client, daemon, options)
	var recovery *pueue.PrivateRecoveryError
	if errors.As(err, &recovery) {
		if recovery.ReasonCode == "transition_pending" || (explicitMigration && recovery.ReasonCode == "published_pair_unavailable_live") {
			return nil // Preserve the existing bootstrap/migration recovery owner.
		}
	}
	return err
}

func resolveReconciliationPair(root string, published task.SupervisorRef, explicitMigration bool) (string, string, bool, error) {
	client, daemon, err := resolveRetainedSupervisorPair(root, published)
	if err != nil {
		pending, restartErr := pueue.PrivatePairRestartPending(root)
		if restartErr != nil {
			return "", "", false, &pueue.PrivateRecoveryError{ReasonCode: "state_unreadable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err, restartErr)}
		}
		if pending {
			return "", "", false, &pueue.PrivateRecoveryError{ReasonCode: "predecessor_pair_unavailable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err, restartErr)}
		}
		if explicitMigration || errors.Is(err, os.ErrNotExist) {
			return "", "", false, nil // Optional repair does not replace explicit migration.
		}
		return "", "", false, &pueue.PrivateRecoveryError{ReasonCode: "state_unreadable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)}
	}
	return client, daemon, true, nil
}

func inspectionAllowsPairReconciliation(ctx context.Context, store *taskdir.Store, published task.SupervisorRef) (bool, error) {
	pending, err := inspection.GroupMigrationPending(ctx, store)
	if err != nil || pending {
		return false, err // The existing inspection transition must finish first.
	}
	head, _, err := inspection.ReadGroupHead(ctx, store)
	if task.RecordAbsent(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return task.SameSupervisorIdentity(head.Supervisor, published), nil
}

func verifyAvailablePublishedSupervisorPair(ctx context.Context, root string, options pueue.Options) (bool, error) {
	current, prepared, err := pueue.PreparePublishedPrivateWithLock(ctx, root, options)
	if err == nil && prepared {
		return true, current.Close()
	}
	if current != nil {
		err = errors.Join(err, current.CloseAfterPending(err))
	}
	if errors.Is(err, pueue.ErrInFlight) || ctx.Err() != nil {
		return false, err
	}
	// An unavailable/incomplete published pair enters the single recovery gate
	// below. That gate rereads its authority and retains the classified cause.
	return false, nil
}

func resolveRetainedSupervisorPair(root string, published task.SupervisorRef) (string, string, error) {
	base := filepath.Join(root, ".supervisor")
	pair := filepath.Join(base, stateSupervisorPairDirectoryPrefix+stateSupervisorPairDigest(published.ClientSHA256, published.DaemonSHA256))
	candidate := published
	candidate.ClientExecutable = filepath.Join(pair, "pueue")
	candidate.DaemonExecutable = filepath.Join(pair, "pueued")
	if err := validateSavedStateSupervisorPairContents(base, pair, candidate); err != nil {
		return "", "", err
	}
	return candidate.ClientExecutable, candidate.DaemonExecutable, nil
}

func reconcileSavedSupervisor(ctx context.Context, root string, options pueue.Options) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if _, bounded := ctx.Deadline(); !bounded {
		boundedContext, cancel := context.WithTimeout(ctx, pueue.PrivateControlTimeout(options))
		defer cancel()
		ctx = boundedContext
	}
	store, err := openStoreContext(ctx, root, Dependencies{}.storeDependencies(), false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	return pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		return reconcilePublishedSupervisorWithLock(lockedContext, store, options, false)
	})
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
	if err = validateSavedStateSupervisorPair(store.Root, recorded); err != nil {
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
		err = inspection.PrepareGroupMigration(ctx, store, head, privateInspectionGroup(store.RootID, current.Binding()))
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
