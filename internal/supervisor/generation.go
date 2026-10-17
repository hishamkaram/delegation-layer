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

// InspectionGroup describes the canonical serial inspection group.
func InspectionGroup(rootID string, supervisor task.SupervisorRef) inspection.GroupRequestRecord {
	return inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		Supervisor:    supervisor,
		Name:          (pueue.InspectionIdentity{RootID: rootID}).Group(), ParallelTasks: 1,
	}
}

// ResolveGeneration observes the active private generation under its lease.
func ResolveGeneration(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (Generation, error) {
	if ctx == nil {
		return Generation{}, context.Canceled
	}
	if store == nil {
		return Generation{}, errors.New("missing private supervisor generation store")
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
		return Generation{}, err
	}
	current, retryEntered, retryErr := resolvePrivateSupervisorGenerationUnderLease(ctx, store, saved)
	if retryEntered || !errors.Is(retryErr, pueue.ErrPrivateSupervisorNotInitialized) {
		return current, retryErr
	}
	return resolution, nil
}

func resolvePrivateSupervisorGenerationUnderLease(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (resolution Generation, entered bool, resultErr error) {
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

// ResolveGenerationWithLock resolves under the caller's lifecycle lock or lease.
func ResolveGenerationWithLock(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef) (Generation, error) {
	return resolvePrivateSupervisorGenerationUsing(ctx, store, saved, pueue.ResolvePrivateSupervisorSuccessorWithLock)
}

func resolvePrivateSupervisorGenerationUsing(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, resolve privateSuccessorResolver) (Generation, error) {
	if ctx == nil {
		return Generation{}, context.Canceled
	}
	if store == nil || resolve == nil {
		return Generation{}, errors.New("missing private supervisor generation resolver")
	}

	authority, err := resolvePrivateSupervisorAuthority(ctx, store.Root, saved, resolve)
	if err != nil {
		return Generation{}, err
	}
	head, migrated, groupPresent, err := readPrivateInspectionGroupHead(ctx, store)
	if err != nil {
		return Generation{}, err
	}

	resolution := Generation{
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

func reconcilePrivateInspectionGroup(ctx context.Context, stateRoot string, resolve privateSuccessorResolver, privateInitialized bool, resolution Generation) (Generation, error) {
	if !pueue.IsPrivateConfig(stateRoot, resolution.GroupHead.Supervisor.ConfigPath) {
		return Generation{}, fmt.Errorf("%w: %w", inspection.ErrGroupMigrationRequired, ErrExternalInspectionHandoffRequired)
	}
	if !resolution.Found && privateInitialized {
		// The caller's saved generation may predate retained history. Resolve the
		// inspection predecessor against the same private authority before using
		// the group journal as a compatibility fallback.
		active, found, err := resolve(ctx, stateRoot, resolution.GroupHead.Supervisor)
		if err != nil {
			return Generation{}, err
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

	connected, connectedFound, connectedErr := resolve(ctx, stateRoot, resolution.GroupHead.Supervisor)
	if connectedErr != nil {
		return Generation{}, connectedErr
	}
	if !connectedFound || connected != resolution.Active {
		return Generation{}, fmt.Errorf("%w: inspection predecessor is not connected to the active private generation", inspection.ErrGroupMigrationRequired)
	}
	resolution.GroupCleanupDue = true
	return resolution, nil
}
