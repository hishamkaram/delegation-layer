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
// the retained pair is derived from the published content identity.
func ReconcilePublishedSupervisorWithLock(ctx context.Context, store *taskdir.Store, options pueue.Options, explicitMigration bool) error {
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
