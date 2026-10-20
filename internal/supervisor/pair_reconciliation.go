package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// reconcilePublishedSupervisorWithLock is the single application eligibility
// gate for path-only recovery. Inventory cannot select the active generation;
// the retained pair is derived from the published content identity. It reads
// control's typed facts and never reclassifies an operational error.
func ReconcilePublishedSupervisorWithLock(ctx context.Context, store *taskdir.Store, options pueue.Options, explicitMigration bool) error {
	// A healthy published pair does not depend on staged or retained inventory.
	// Its availability is decided before any relocation candidate.
	if settled, err := publishedPairAvailabilitySettles(ctx, store, options, explicitMigration); settled {
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
	outcome, err := pueue.ReconcilePublishedPrivatePairAttemptWithLock(ctx, store.Root, client, daemon, options)
	if err != nil {
		return err
	}
	switch outcome {
	case pueue.PublishedPairReconciled:
	case pueue.PublishedPairTransitionPending:
		// Preserve the existing bootstrap/migration recovery owner.
	case pueue.PublishedPairLiveUnavailable:
		if !explicitMigration {
			return outcome.ProjectedError()
		}
		// Explicit migration owns a live predecessor.
	}
	return nil
}

// publishedPairAvailabilitySettles applies control's availability fact and
// reports whether it settles the call. Any observation fault settles with its
// error; only a member-missing or incomplete generation continues.
func publishedPairAvailabilitySettles(ctx context.Context, store *taskdir.Store, options pueue.Options, explicitMigration bool) (bool, error) {
	availability, err := pueue.ObservePublishedPrivatePairWithLock(ctx, store.Root, options)
	switch {
	case err != nil:
		return true, err
	case availability == pueue.PublishedPairReady || availability == pueue.PublishedPairNotPublished:
		return true, nil
	case availability == pueue.PublishedPairConfigChanged && explicitMigration:
		// Both executables proved their published provenance, so path-only
		// repair has nothing to do. Explicit migration owns the config change.
		return true, nil
	case availability == pueue.PublishedPairConfigChanged:
		return true, availability.ProjectedError()
	}
	return false, nil
}

func resolveReconciliationPair(root string, published task.SupervisorRef, explicitMigration bool) (string, string, bool, error) {
	client, daemon, err := ResolveRetainedSupervisorPair(root, published)
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
