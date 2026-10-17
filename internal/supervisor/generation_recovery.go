package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// CompletePendingGroupMigrationWithLock publishes only the exact pending
// inspection edge after Pueue proves that the same private edge is complete.
// The caller owns the state-root bootstrap lock throughout this operation.
func CompletePendingGroupMigrationWithLock(ctx context.Context, store *taskdir.Store, resolution Generation) error {
	if !resolution.GroupCleanupDue {
		return nil
	}
	successor := InspectionGroup(store.RootID, resolution.Active)
	_, complete, err := PrivateMigrationPredecessorWithLock(
		ctx,
		store.Root,
		resolution.GroupHead.Supervisor,
		resolution.Active,
	)
	if err != nil {
		return fmt.Errorf("%w: verify completed private supervisor edge: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !complete {
		return fmt.Errorf("%w: private supervisor edge is not complete", inspection.ErrGroupMigrationRequired)
	}
	if err = inspection.CompleteGroupMigration(ctx, store, resolution.GroupHead, successor); err != nil {
		return fmt.Errorf("%w: publish exact inspection group edge: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}

// An inspection head retains immutable launch provenance across same-logical
// private upgrades. The canonical successor resolver selects the actual
// predecessor or proves the completed target; the head need not name the
// immediately preceding private history edge.
func PrivateMigrationPredecessorWithLock(ctx context.Context, root string, predecessor, successor task.SupervisorRef) (task.SupervisorRef, bool, error) {
	if task.SameSupervisorIdentity(predecessor, successor) {
		complete, err := pueue.PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, root, predecessor, successor)
		return predecessor, complete, err
	}
	current, found, err := pueue.ResolvePrivateSupervisorSuccessorWithLock(ctx, root, predecessor)
	if errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		return predecessor, false, nil // The existing private protocol owns resumption.
	}
	if err != nil || !found {
		return predecessor, false, err
	}
	if current == successor {
		return predecessor, true, nil
	}
	if task.SameSupervisorIdentity(current, predecessor) {
		return current, false, nil
	}
	return task.SupervisorRef{}, false, fmt.Errorf("%w: prepared target is not the active inspection successor", inspection.ErrGroupMigrationRequired)
}

// RecoverInspectionGroupGenerationWithLock finishes the exact group edge under the caller's bootstrap lock.
func RecoverInspectionGroupGenerationWithLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, requireActiveClient bool) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private inspection supervisor")
	}
	resolution, err := ResolveGenerationWithLock(ctx, store, supervisor.Binding())
	if errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		// A completed group head may request retirement of a recovery alias, but
		// it does not select the generation. RetirePrivateRecoveryBinding proves
		// the published current binding and exact private history before removal;
		// only then can the canonical resolver be retried.
		if cleanupErr := ClearCompletedInspectionRecovery(ctx, store, supervisor); cleanupErr != nil {
			return cleanupErr
		}
		resolution, err = ResolveGenerationWithLock(ctx, store, supervisor.Binding())
	}
	if err != nil {
		return err
	}
	if requireActiveClient && resolution.PrivateAuthority && resolution.Active != supervisor.Binding() {
		return fmt.Errorf("%w: inspection client is not bound to the active private generation", inspection.ErrGroupMigrationRequired)
	}
	return CompletePendingGroupMigrationWithLock(ctx, store, resolution)
}

// ClearCompletedInspectionRecovery retires only a proven completed recovery alias.
func ClearCompletedInspectionRecovery(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	head, migrated, err := inspection.ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !migrated || !task.SameSupervisorIdentity(head.Supervisor, supervisor.Binding()) {
		return nil
	}
	return pueue.RetirePrivateRecoveryBindingContext(ctx, store.Root, supervisor.Binding())
}
