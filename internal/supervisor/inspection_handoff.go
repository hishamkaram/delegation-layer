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

// resolvePrivateAdmissionMigrationWithBootstrapLock repairs only the routing
// journal for a caller that selected the private supervisor. The caller holds
// the root bootstrap lock; private daemon migrations keep their explicit gate.
func resolvePrivateAdmissionMigrationWithBootstrapLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted, endpointProvenLive bool) error {
	err := RejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, endpointProvenLive)
	if !errors.Is(err, ErrExternalInspectionHandoffRequired) {
		return err
	}
	legacy, _, err := inspection.ReadGroupHead(ctx, store)
	if err != nil {
		return err
	}
	if err = HandoffExternalInspectionGroup(ctx, store, supervisor, legacy, InspectionGroup(store.RootID, supervisor.Binding())); err != nil {
		return err
	}
	return RejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, endpointProvenLive)
}

// handoffExternalInspectionGroup changes only the inspection journal's head.
// Saved task and inspection bindings remain valid on the external supervisor.
// The caller holds the root bootstrap lock; no daemon migration is authorized.
func HandoffExternalInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, legacy, successor inspection.GroupRequestRecord) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil || supervisor == nil {
		return task.ErrEvidenceFault
	}
	if pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) || !pueue.IsPrivateConfig(store.Root, successor.Supervisor.ConfigPath) {
		return inspection.ErrGroupMigrationUnsupported
	}
	if successor != InspectionGroup(store.RootID, supervisor.Binding()) || legacy.RootID != store.RootID {
		return task.ErrEvidenceFault
	}
	if err := supervisor.StartPrivate(ctx, store.Root); err != nil {
		return fmt.Errorf("%w: prepare private inspection successor: %w", inspection.ErrGroupMigrationRequired, err)
	}
	// Publish the private owner before preparing the routing edge. A crash then
	// reselects this exact generation rather than a later staged installation.
	if err := supervisor.EnsurePrivateSupervisorBindingWithLock(ctx, store.Root); err != nil {
		return err
	}
	if err := inspection.PrepareGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: prepare inspection handoff: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err := inspection.CompleteGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: publish inspection handoff: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}
