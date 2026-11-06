package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
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
		matches, matchErr := pueue.PrivateMigrationTargetsWithLock(store.Root, supervisor.Binding())
		if matchErr != nil {
			return matchErr
		}
		retargetable, readErr := pueue.PrivateMigrationRetargetableWithLock(store.Root)
		if readErr != nil {
			return readErr
		}
		if !matches && retargetable {
			// The group callback must append the selected proposal before the
			// private owner replaces its reversible target or stops anything.
			return nil
		}
		if err = supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root); err != nil {
			return err
		}
	} else {
		prepared, prepareErr := reconcileInspectionMigrationEntryWithLock(ctx, store, supervisor)
		if prepareErr != nil {
			return prepareErr
		}
		if prepared {
			// The current head and recovery alias still own the published
			// predecessor. Let the group callback journal its edge before the
			// private owner accepts this different prepared logical target.
			return nil
		}
	}
	return RecoverInspectionGroupGenerationWithLock(ctx, store, supervisor, false)
}

func reconcileInspectionMigrationEntryWithLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) (bool, error) {
	head, _, err := inspection.ReadGroupHead(ctx, store)
	headPresent := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	published, privateFound, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil {
		return false, err
	}
	alias, aliasFound, err := pueue.ReadPrivateRecoveryBinding(store.Root)
	if err != nil {
		return false, err
	}
	if !headPresent || !privateFound || !aliasFound {
		return false, nil
	}
	candidate := supervisor.Binding()
	if task.SameSupervisorIdentity(head.Supervisor, published) &&
		task.SameSupervisorIdentity(alias, published) &&
		!task.SameSupervisorIdentity(published, candidate) {
		return true, nil
	}
	if pueue.IsPrivateConfig(store.Root, head.Supervisor.ConfigPath) &&
		sameInspectionMigrationTarget(published, candidate) && !task.SameSupervisorIdentity(alias, published) {
		// The private owner validates the completed history and retires the
		// predecessor alias before unchanged inspection recovery resolves B.
		return false, supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root)
	}
	return false, nil
}

func sameInspectionMigrationTarget(published, candidate task.SupervisorRef) bool {
	return task.SameSupervisorIdentity(published, candidate) &&
		published.ClientExecutable == candidate.ClientExecutable &&
		published.DaemonExecutable == candidate.DaemonExecutable &&
		published.ExecutableIdentity == candidate.ExecutableIdentity
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
		if err := supervisor.PublishVerifiedPrivatePredecessorWithLock(ctx, store.Root, head.Supervisor); err != nil {
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
	if err = ReconcileStaleInspectionRecovery(ctx, store, supervisor, legacy); err != nil {
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
		if err = supervisor.PublishVerifiedPrivatePredecessorWithLock(ctx, stateRoot, predecessor); err != nil {
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

func ReconcileStaleInspectionRecovery(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, legacy inspection.GroupRequestRecord) error {
	if store == nil || supervisor == nil {
		return fmt.Errorf("%w: inspection state store or supervisor is unavailable", inspection.ErrGroupMigrationRequired)
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
	if err = supervisor.ClearPrivateRecoveryBindingWithLock(ctx, store.Root, saved); err != nil {
		return fmt.Errorf("%w: retire stale private supervisor recovery: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}
