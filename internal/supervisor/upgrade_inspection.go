package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// UpgradeInspectionCleanupDue is passive. In particular historical A while B
// owns the root is replayed without opening B's migration or installed pair.
func UpgradeInspectionCleanupDue(ctx context.Context, store *taskdir.Store, operation pueue.UpgradeOperationID) (due bool, resultErr error) {
	if store == nil {
		return false, task.ErrInvalidPermit
	}
	inspect := func(locked context.Context) error {
		pin, err := store.PinRootContext(locked)
		if err != nil {
			return err
		}
		target, eligible, err := pueue.PrivateUpgradeInspectionTargetWithLock(locked, pin, operation)
		if err != nil || !eligible {
			return err
		}
		generation, err := ResolveGenerationWithLock(locked, store, target)
		due = generation.GroupCleanupDue
		return err
	}
	if pueue.RequireRuntimeRestartBootstrap(ctx, store.Root) == nil {
		resultErr = inspect(ctx)
		return due, resultErr
	}
	resultErr = pueue.WithSupervisorLeaseContext(ctx, store.Root, inspect)
	return due, resultErr
}

// CompleteUpgradeInspectionWithLock writes only the derived inspection edge.
// The resident owns EX and the generation commit guard; private control retains
// A, its exact target and all daemon Start/Wait authority. Empty A is ordinary
// current-generation maintenance, never permission to begin an upgrade.
func CompleteUpgradeInspectionWithLock(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, operation pueue.UpgradeOperationID, client *pueue.Client, guard pueue.PrivateGenerationGuard) error {
	if store == nil || client == nil || guard == nil {
		return ErrInfrastructureAuthorityUnavailable
	}
	if err := pueue.RequireRuntimeRestartBootstrap(ctx, store.Root); err != nil {
		return err
	}
	generation, err := upgradeInspectionGeneration(ctx, store, pin, operation, client.Binding())
	if err != nil || !generation.GroupCleanupDue {
		return err
	}
	if err = inspectPublishedMigrationQueue(ctx, store, client); err != nil {
		return err
	}
	return guard.GuardPrivateGenerationCommit(ctx, store.Root, func() error {
		current, checkErr := upgradeInspectionGeneration(ctx, store, pin, operation, client.Binding())
		if checkErr != nil || current != generation {
			return errors.Join(task.ErrIdentityMismatch, checkErr)
		}
		successor := InspectionGroup(store.RootID, generation.Active)
		if err := inspection.PrepareGroupMigration(ctx, store, generation.GroupHead, successor); err != nil {
			return err
		}
		return inspection.CompleteGroupMigration(ctx, store, generation.GroupHead, successor)
	})
}

func upgradeInspectionGeneration(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, operation pueue.UpgradeOperationID, binding task.SupervisorRef) (Generation, error) {
	target, eligible, err := pueue.PrivateUpgradeInspectionTargetWithLock(ctx, pin, operation)
	if err != nil || !eligible || target != binding || pin.Root() != store.Root || pin.RootID() != store.RootID {
		return Generation{}, errors.Join(task.ErrIdentityMismatch, err)
	}
	generation, err := ResolveGenerationWithLock(ctx, store, target)
	if err != nil {
		return Generation{}, err
	}
	if !generation.PrivateAuthority || generation.Active != target {
		return Generation{}, task.ErrIdentityMismatch
	}
	if !generation.GroupCleanupDue {
		return generation, nil
	}
	_, complete, err := PrivateMigrationPredecessorWithLock(ctx, store.Root, generation.GroupHead.Supervisor, target)
	if err != nil || !complete {
		return Generation{}, errors.Join(task.ErrEvidenceFault, err)
	}
	proposals, err := inspection.PreparedGroupMigrationSuccessors(ctx, store)
	if err != nil {
		return Generation{}, err
	}
	for _, proposal := range proposals {
		if proposal != target {
			return Generation{}, fmt.Errorf("%w: conflicting inspection migration proposal", task.ErrEvidenceFault)
		}
	}
	return generation, nil
}
