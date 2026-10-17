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

func EnsureInspectionGroup(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (resultErr error) {
	return EnsureInspectionGroupWithMigration(store, supervisor, timeout, false)
}

// ensureInspectionGroupWithoutMigration binds a historical continuation to
// the exact inspection generation it recorded. A continuation must never
// rewrite the root journal backwards to its predecessor supervisor.
func EnsureInspectionGroupWithoutMigration(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (resultErr error) {
	if store == nil || supervisor == nil {
		return errors.New("missing inspection supervisor")
	}
	if timeout <= 0 {
		return pueue.ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return ensureInspectionGroupHistorical(ctx, store, supervisor)
	}
	if ready, probeErr := probeEstablishedInspectionGroup(ctx, store, supervisor); probeErr != nil || ready {
		return probeErr
	}
	return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
		return ensureInspectionGroupHistorical(lockedContext, store, supervisor)
	})
}

func ensureInspectionGroupHistorical(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) (resultErr error) {
	group, err := inspection.OpenGroupHistorical(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	return ensureInspectionGroupReady(ctx, store, supervisor, group)
}

func EnsureInspectionGroupWithMigration(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	return EnsureInspectionGroupWithMigrationContext(context.Background(), store, supervisor, timeout, allowMigration)
}

func EnsureInspectionGroupWithMigrationContext(parent context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	ctx, cancel, err := inspectionGroupContext(parent, store, supervisor, timeout)
	if err != nil {
		return err
	}
	defer cancel()
	// Ordinary admission never stops or rebinds a reachable supervisor. The
	// private bootstrap path already adopted the endpoint under its bootstrap
	// lock; the group journal has its own rooted transaction lock. Migration is
	// an explicit supervisor repair operation.
	ensure := func(ensureContext context.Context) (ensureErr error) {
		if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
			if err := RejectExternalInspectionMigration(ensureContext, store, supervisor); err != nil {
				return err
			}
		} else if err := RecoverInspectionGroupGenerationWithLock(ensureContext, store, supervisor, true); err != nil {
			return err
		}
		migrationCheck := inspectionGroupMigrationCheck(store, supervisor, timeout, allowMigration)
		group, err := inspection.OpenGroupWithMigration(ensureContext, store, supervisor.Binding(), migrationCheck)
		if err != nil {
			return err
		}
		defer func() { ensureErr = errors.Join(ensureErr, group.Close()) }()
		if err := ensureInspectionGroupReady(ensureContext, store, supervisor, group); err != nil {
			return err
		}
		if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
			if err := ClearCompletedInspectionRecovery(ensureContext, store, supervisor); err != nil {
				return err
			}
		}
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return ensure(ctx)
	}
	if ready, probeErr := probeEstablishedInspectionGroup(ctx, store, supervisor); probeErr != nil || ready {
		return probeErr
	}
	return pueue.WithBootstrapLockContext(ctx, store.Root, ensure)
}

func inspectionGroupContext(parent context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, context.Canceled
	}
	if store == nil || supervisor == nil {
		return nil, nil, errors.New("missing inspection supervisor")
	}
	if timeout <= 0 {
		return nil, nil, pueue.ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, nil
}

func probeEstablishedInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) (bool, error) {
	ready := false
	err := pueue.WithSupervisorLeaseContext(ctx, store.Root, func(lockedContext context.Context) error {
		complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, supervisor.Binding())
		if completeErr != nil || !complete {
			return completeErr
		}
		snapshot, snapshotErr := supervisor.Snapshot(lockedContext)
		if snapshotErr != nil {
			return snapshotErr
		}
		var probeErr error
		ready, probeErr = inspection.ProbeGroupContext(lockedContext, store, supervisor.Binding(), snapshot)
		return probeErr
	})
	if errors.Is(err, pueue.ErrPrivateSupervisorNotInitialized) {
		// This root has no published lifecycle boundary to observe. Let the
		// caller elect the exclusive bootstrap transition; a shared probe must
		// not create the missing lock merely to report it absent.
		return false, nil
	}
	return ready, err
}

func RejectExternalInspectionMigration(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	legacy, _, err := inspection.ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.SameSupervisorIdentity(legacy.Supervisor, supervisor.Binding()) {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	if !pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	return nil
}

func EnsureInspectionGroupLocked(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		if err := RecoverInspectionGroupGenerationWithLock(ctx, store, supervisor, !allowMigration); err != nil {
			return err
		}
	}
	if err := prepareInspectionGroupSupervisor(ctx, store, supervisor, allowMigration); err != nil {
		return err
	}
	migrationCheck := inspectionGroupMigrationCheck(store, supervisor, timeout, allowMigration)
	group, err := inspection.OpenGroupWithMigration(ctx, store, supervisor.Binding(), migrationCheck)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		if err := supervisor.StartPrivate(ctx, store.Root); err != nil {
			return err
		}
		if err := ClearCompletedInspectionRecovery(ctx, store, supervisor); err != nil {
			return err
		}
	}
	return ensureInspectionGroupReady(ctx, store, supervisor, group)
}

func ensureInspectionGroupReady(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, group *inspection.GroupJournal) error {
	snapshot, err := supervisor.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, exists := snapshot.Groups[group.Name()]; exists {
		return group.ObserveContext(ctx, snapshot)
	}
	permit, err := group.ClaimCreateContext(ctx, time.Now())
	if err != nil {
		return err
	}
	_, createErr := supervisor.CreateInspectionGroup(ctx, permit, store.RootID)
	if pending := PendingInspectionCommand(createErr); pending != nil {
		// The create command retains the private supervisor lease until it
		// naturally completes. Returning its pending owner preserves that
		// serialization without extending this caller past its finite deadline.
		return createErr
	}
	// A failed or lost response never permits another create. A fresh exact
	// snapshot can resolve the saved intent, including after an uncertain reply.
	if err = ctx.Err(); err != nil {
		return errors.Join(createErr, err)
	}
	snapshot, observeErr := supervisor.Snapshot(ctx)
	if observeErr != nil {
		return errors.Join(createErr, observeErr)
	}
	if observeErr = group.ObserveContext(ctx, snapshot); observeErr != nil {
		return errors.Join(createErr, observeErr)
	}
	return nil
}
