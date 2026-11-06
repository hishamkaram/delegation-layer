package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The bootstrap and inspection owners decide whether reconciliation may run.
// Installation lookup supplies only a candidate; it cannot replace a recorded
// restart target or authorize a new daemon generation.
func ReconcilePublishedSupervisorWithLock(ctx context.Context, store *taskdir.Store, options pueue.Options, lookup ExecutableLookup, explicitMigration bool) error {
	if _, err := store.PinRootContext(ctx); err != nil {
		return err
	}
	restartPending, err := pueue.PrivatePairRestartPending(store.Root)
	if err != nil {
		return &pueue.PrivateRecoveryError{ReasonCode: "state_unreadable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)}
	}
	if settled, availabilityErr := publishedPairAvailabilitySettles(ctx, store, options, explicitMigration); settled {
		return availabilityErr
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil {
		return err
	}
	if found {
		allowed, inspectionErr := inspectionAllowsPairReconciliation(ctx, store, published)
		if inspectionErr != nil {
			return inspectionErr
		}
		if !allowed {
			if restartPending {
				return pueue.PublishedPairTransitionPending.ProjectedError()
			}
			return nil
		}
	}
	outcome, err := pueue.ReconcilePublishedPrivatePairAttemptWithLock(ctx, store.Root, func(selection context.Context) (string, string, error) {
		return resolveCompatiblePrivatePair(selection, store.Root, lookup)
	}, options)
	if err != nil {
		if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, pueue.ErrCompatiblePairUnavailable) }) {
			return nil
		}
		return err
	}
	if outcome == pueue.PublishedPairLiveUnavailable {
		return outcome.ProjectedError()
	}
	if outcome == pueue.PublishedPairTransitionPending && restartPending {
		return outcome.ProjectedError()
	}
	// Readable competing transitions keep their existing recovery owner.
	return nil
}

func publishedPairAvailabilitySettles(ctx context.Context, store *taskdir.Store, options pueue.Options, explicitMigration bool) (bool, error) {
	availability, err := pueue.ObservePublishedPrivatePairWithLock(ctx, store.Root, options)
	switch {
	case err != nil:
		return true, err
	case availability == pueue.PublishedPairReady || availability == pueue.PublishedPairNotPublished:
		return true, nil
	case explicitMigration && (availability == pueue.PublishedPairConfigChanged || availability == pueue.PublishedPairMemberMissing):
		// Explicit migration owns replacement after its queue and history gates.
		return true, nil
	case availability == pueue.PublishedPairConfigChanged:
		return true, availability.ProjectedError()
	}
	return false, nil
}

func inspectionAllowsPairReconciliation(ctx context.Context, store *taskdir.Store, published task.SupervisorRef) (bool, error) {
	pending, err := inspection.GroupMigrationPending(ctx, store)
	if err != nil || pending {
		return false, err
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
