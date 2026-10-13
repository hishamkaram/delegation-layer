package app

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

// privateSupervisorGeneration keeps one authoritative owner for the active
// private generation. Pueue's completed lifecycle journal selects Active; the
// inspection journal can only identify an exact edge that still needs its
// publication completed.
type privateSupervisorGeneration struct {
	Active           task.SupervisorRef
	Found            bool
	PrivateAuthority bool
	GroupHead        inspection.GroupRequestRecord
	GroupPresent     bool
	GroupMigrated    bool
	GroupCleanupDue  bool
}

type privateSuccessorResolver func(context.Context, string, task.SupervisorRef) (task.SupervisorRef, bool, error)

type privateSupervisorAuthority struct {
	Active      task.SupervisorRef
	Found       bool
	Initialized bool
}

func resolvePrivateSupervisorGeneration(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (privateSupervisorGeneration, error) {
	if ctx == nil {
		return privateSupervisorGeneration{}, context.Canceled
	}
	if store == nil {
		return privateSupervisorGeneration{}, errors.New("missing private supervisor generation store")
	}
	resolution, leaseEntered, err := resolvePrivateSupervisorGenerationUnderLease(ctx, store, saved)
	if leaseEntered || !errors.Is(err, pueue.ErrPrivateSupervisorNotInitialized) {
		return resolution, err
	}

	// A root created before the private lifecycle journal existed can still have
	// an authoritative inspection-group successor. Read that compatibility state
	// without manufacturing a supervisor lock, then re-probe the lifecycle
	// boundary. If initialization won the race, its generation replaces the
	// compatibility snapshot; otherwise the second absent observation is the
	// linearization point for returning the group-only result.
	resolution, err = resolvePrivateSupervisorGenerationUsing(ctx, store, saved, privateSupervisorLifecycleUnavailable)
	if err != nil {
		return privateSupervisorGeneration{}, err
	}
	current, retryEntered, retryErr := resolvePrivateSupervisorGenerationUnderLease(ctx, store, saved)
	if retryEntered || !errors.Is(retryErr, pueue.ErrPrivateSupervisorNotInitialized) {
		return current, retryErr
	}
	return resolution, nil
}

func resolvePrivateSupervisorGenerationUnderLease(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (resolution privateSupervisorGeneration, entered bool, resultErr error) {
	resultErr = pueue.WithSupervisorLeaseContext(ctx, store.Root, func(lockedContext context.Context) error {
		entered = true
		var err error
		resolution, err = resolvePrivateSupervisorGenerationUsing(lockedContext, store, saved, pueue.ResolvePrivateSupervisorSuccessorWithLock)
		return err
	})
	return resolution, entered, resultErr
}

func privateSupervisorLifecycleUnavailable(context.Context, string, task.SupervisorRef) (task.SupervisorRef, bool, error) {
	return task.SupervisorRef{}, false, pueue.ErrPrivateSupervisorNotInitialized
}

func resolvePrivateSupervisorGenerationWithLock(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (privateSupervisorGeneration, error) {
	return resolvePrivateSupervisorGenerationUsing(ctx, store, saved, pueue.ResolvePrivateSupervisorSuccessorWithLock)
}

func resolvePrivateSupervisorGenerationUsing(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, resolve privateSuccessorResolver) (privateSupervisorGeneration, error) {
	if ctx == nil {
		return privateSupervisorGeneration{}, context.Canceled
	}
	if store == nil || resolve == nil {
		return privateSupervisorGeneration{}, errors.New("missing private supervisor generation resolver")
	}

	authority, err := resolvePrivateSupervisorAuthority(ctx, store.Root, saved, resolve)
	if err != nil {
		return privateSupervisorGeneration{}, err
	}
	head, migrated, groupPresent, err := readPrivateInspectionGroupHead(ctx, store)
	if err != nil {
		return privateSupervisorGeneration{}, err
	}

	resolution := privateSupervisorGeneration{
		Active:           authority.Active,
		Found:            authority.Found,
		PrivateAuthority: authority.Found,
		GroupHead:        head,
		GroupPresent:     groupPresent,
		GroupMigrated:    migrated,
	}
	if !groupPresent {
		return resolution, nil
	}
	return reconcilePrivateInspectionGroup(ctx, store.Root, resolve, authority.Initialized, resolution)
}

func resolvePrivateSupervisorAuthority(ctx context.Context, stateRoot string, saved task.SupervisorRef, resolve privateSuccessorResolver) (privateSupervisorAuthority, error) {
	// This lookup must precede the inspection head read. A completed private
	// migration is the active-generation authority even if its group edge was
	// interrupted immediately before publication.
	active, found, err := resolve(ctx, stateRoot, saved)
	initialized := !errors.Is(err, pueue.ErrPrivateSupervisorNotInitialized)
	if err != nil && initialized {
		return privateSupervisorAuthority{}, err
	}
	return privateSupervisorAuthority{Active: active, Found: found, Initialized: initialized}, nil
}

func readPrivateInspectionGroupHead(ctx context.Context, store *taskdir.Store) (inspection.GroupRequestRecord, bool, bool, error) {
	head, migrated, err := inspection.ReadGroupHead(ctx, store)
	if err == nil {
		return head, migrated, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return inspection.GroupRequestRecord{}, false, false, nil
	}
	return inspection.GroupRequestRecord{}, false, false, err
}

func reconcilePrivateInspectionGroup(ctx context.Context, stateRoot string, resolve privateSuccessorResolver, privateInitialized bool, resolution privateSupervisorGeneration) (privateSupervisorGeneration, error) {
	if !resolution.Found && privateInitialized {
		// The caller's saved generation may predate retained history. Resolve the
		// inspection predecessor against the same private authority before using
		// the group journal as a compatibility fallback.
		active, found, err := resolve(ctx, stateRoot, resolution.GroupHead.Supervisor)
		if err != nil {
			return privateSupervisorGeneration{}, err
		}
		resolution.Active = active
		resolution.Found = found
		resolution.PrivateAuthority = found
	}
	if !resolution.Found {
		resolution.Active = resolution.GroupHead.Supervisor
		resolution.Found = true
		return resolution, nil
	}
	if task.SameSupervisorIdentity(resolution.GroupHead.Supervisor, resolution.Active) {
		return resolution, nil
	}
	if !pueue.IsPrivateConfig(stateRoot, resolution.GroupHead.Supervisor.ConfigPath) {
		return privateSupervisorGeneration{}, fmt.Errorf("%w: inspection predecessor is external", inspection.ErrGroupMigrationUnsupported)
	}

	connected, connectedFound, connectedErr := resolve(ctx, stateRoot, resolution.GroupHead.Supervisor)
	if connectedErr != nil {
		return privateSupervisorGeneration{}, connectedErr
	}
	if !connectedFound || connected != resolution.Active {
		return privateSupervisorGeneration{}, fmt.Errorf("%w: inspection predecessor is not connected to the active private generation", inspection.ErrGroupMigrationRequired)
	}
	resolution.GroupCleanupDue = true
	return resolution, nil
}

// completePendingGroupMigrationWithLock publishes only the exact pending
// inspection edge after Pueue proves that the same private edge is complete.
// The caller owns the state-root bootstrap lock throughout this operation.
func completePendingGroupMigrationWithLock(ctx context.Context, store *taskdir.Store, resolution privateSupervisorGeneration) error {
	if !resolution.GroupCleanupDue {
		return nil
	}
	successor := privateInspectionGroup(store.RootID, resolution.Active)
	complete, err := pueue.PrivateSupervisorMigrationEdgeCompleteWithLock(
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

func recoverInspectionGroupGenerationWithLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private inspection supervisor")
	}
	resolution, err := resolvePrivateSupervisorGenerationWithLock(ctx, store, supervisor.Binding())
	if errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		// A completed group head may request retirement of a recovery alias, but
		// it does not select the generation. RetirePrivateRecoveryBinding proves
		// the published current binding and exact private history before removal;
		// only then can the canonical resolver be retried.
		if cleanupErr := clearCompletedInspectionRecovery(ctx, store, supervisor); cleanupErr != nil {
			return cleanupErr
		}
		resolution, err = resolvePrivateSupervisorGenerationWithLock(ctx, store, supervisor.Binding())
	}
	if err != nil {
		return err
	}
	if resolution.PrivateAuthority && resolution.Active != supervisor.Binding() {
		return fmt.Errorf("%w: inspection client is not bound to the active private generation", inspection.ErrGroupMigrationRequired)
	}
	return completePendingGroupMigrationWithLock(ctx, store, resolution)
}
