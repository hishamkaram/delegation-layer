package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func MigrateInspectionSupervisorLocked(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) error {
	if store == nil || supervisor == nil {
		return errors.New("missing inspection supervisor")
	}
	if err := resumeInspectionSupervisorGenerationWithLock(ctx, store, supervisor); err != nil {
		return err
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if present && matches {
		if err = prepareInspectionGroupSupervisor(ctx, store, supervisor, true); err != nil {
			return err
		}
	}
	if !present || matches {
		if migrationErr := supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root); migrationErr != nil {
			return migrationErr
		}
	}
	if !present {
		return nil
	}
	group, err := inspection.OpenGroupWithMigration(ctx, store, supervisor.Binding(), func(checkCtx context.Context, legacy inspection.GroupRequestRecord) error {
		return MigrateInspectionGroup(checkCtx, store, supervisor, timeout, legacy)
	})
	if err != nil {
		return err
	}
	return group.Close()
}

func resumeInspectionSupervisorGenerationWithLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	pending, err := pueue.PrivateSupervisorMigrationPending(store.Root)
	if err != nil {
		return err
	}
	if pending {
		if err = supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root); err != nil {
			return err
		}
	}
	return RecoverInspectionGroupGenerationWithLock(ctx, store, supervisor, false)
}

func prepareInspectionGroupSupervisor(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, allowMigration bool) error {
	if !allowMigration || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if present && matches {
		head, _, readErr := inspection.ReadGroupHead(ctx, store)
		if readErr != nil {
			return readErr
		}
		if err := pueue.PublishVerifiedPrivatePredecessorWithLock(ctx, store.Root, head.Supervisor); err != nil {
			return err
		}
		// A completed logical migration is the durable proof that the
		// predecessor endpoint has already been retired. Reconcile its
		// recovery alias before daemon-provenance repair so a retry cannot
		// mistake the completed transition for a fresh legacy binding.
		if err := ClearCompletedInspectionRecovery(ctx, store, supervisor); err != nil {
			return err
		}
	}
	if !present {
		return supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root)
	}
	// A stale inspection head is the authority for the legacy transition. Its
	// queue must be inspected before private migration starts a successor; the
	// migration callback performs that ordering while this supervisor remains
	// prepared but stopped.
	return nil
}

func inspectionGroupMigrationCheck(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) func(context.Context, inspection.GroupRequestRecord) error {
	if !allowMigration {
		return nil
	}
	return func(ctx context.Context, legacy inspection.GroupRequestRecord) error {
		return MigrateInspectionGroup(ctx, store, supervisor, timeout, legacy)
	}
}

func MigrateInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, legacy inspection.GroupRequestRecord) error {
	successor := InspectionGroup(store.RootID, supervisor.Binding())
	complete, err := inspection.GroupMigrationComplete(ctx, store, legacy, successor)
	if err != nil {
		return fmt.Errorf("%w: read supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if complete {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) && pueue.IsPrivateConfig(store.Root, successor.Supervisor.ConfigPath) {
		return HandoffExternalInspectionGroup(ctx, store, supervisor, legacy, successor)
	}
	if !pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) || !pueue.IsPrivateConfig(store.Root, successor.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: only Delegate private supervisors can be migrated", inspection.ErrGroupMigrationUnsupported)
	}
	if err = ReconcileStaleInspectionRecovery(ctx, store, legacy); err != nil {
		return err
	}
	if err = inspection.PrepareGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: prepare supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = ensureInspectionPrivateMigration(ctx, store.Root, supervisor, legacy.Supervisor, successor.Supervisor); err != nil {
		return err
	}
	if err := inspection.CompleteGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: publish supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}

func ensureInspectionPrivateMigration(ctx context.Context, stateRoot string, supervisor *pueue.Client, predecessor, successor task.SupervisorRef) error {
	predecessor, complete, err := PrivateMigrationPredecessorWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify completed private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if complete {
		return nil
	}
	published, err := pueue.PrivateSupervisorMigrationEdgePublishedWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify published private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !published {
		if err = pueue.PublishVerifiedPrivatePredecessorWithLock(ctx, stateRoot, predecessor); err != nil {
			return fmt.Errorf("%w: preserve legacy supervisor binding: %w", inspection.ErrGroupMigrationRequired, err)
		}
	}
	if err = supervisor.MigratePrivateSupervisorWithLock(ctx, stateRoot); err != nil {
		return fmt.Errorf("%w: migrate private supervisor with legacy inspection evidence: %w", inspection.ErrGroupMigrationRequired, err)
	}
	_, complete, err = PrivateMigrationPredecessorWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify reconciled private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !complete {
		return fmt.Errorf("%w: reconciled private supervisor migration remains incomplete", inspection.ErrGroupMigrationRequired)
	}
	return nil
}

func ReconcileStaleInspectionRecovery(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord) error {
	if store == nil {
		return fmt.Errorf("%w: inspection state store is unavailable", inspection.ErrGroupMigrationRequired)
	}
	saved, found, err := pueue.ReadPrivateRecoveryBinding(store.Root)
	if err != nil || !found {
		return err
	}
	if task.SameSupervisorIdentity(saved, legacy.Supervisor) {
		return nil
	}
	historical, err := inspection.OpenGroupHistorical(ctx, store, saved)
	if err != nil {
		return fmt.Errorf("%w: stale private supervisor recovery is not a published predecessor: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = historical.Close(); err != nil {
		return fmt.Errorf("%w: close stale private supervisor history: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = pueue.ClearPrivateRecoveryBinding(store.Root, saved); err != nil {
		return fmt.Errorf("%w: retire stale private supervisor recovery: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}
