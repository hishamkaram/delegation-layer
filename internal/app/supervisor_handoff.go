package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// prepareInspectionHandoffSupervisor keeps the published private generation
// authoritative across a handoff retry, even when a newer pair has been staged.
// The caller holds the root bootstrap lock.
func prepareInspectionHandoffSupervisor(ctx context.Context, root string, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supervisor, found, err := pueue.PreparePublishedPrivateWithLock(ctx, root, options)
	if err != nil || found {
		return supervisor, err
	}
	return prepareCurrentPrivateSupervisor(ctx, root, deps, options)
}

// resolvePrivateAdmissionMigrationWithBootstrapLock repairs only the routing
// journal for a caller that selected the private supervisor. The caller holds
// the root bootstrap lock; private daemon migrations keep their explicit gate.
func resolvePrivateAdmissionMigrationWithBootstrapLock(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted, endpointProvenLive bool) error {
	err := rejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, endpointProvenLive)
	if !errors.Is(err, errExternalInspectionHandoffRequired) {
		return err
	}
	legacy, _, err := inspection.ReadGroupHead(ctx, store)
	if err != nil {
		return err
	}
	if err = handoffExternalInspectionGroup(ctx, store, supervisor, legacy, privateInspectionGroup(store.RootID, supervisor.Binding())); err != nil {
		return err
	}
	return rejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, endpointProvenLive)
}

// handoffExternalInspectionGroup changes only the inspection journal's head.
// Saved task and inspection bindings remain valid on the external supervisor.
// The caller holds the root bootstrap lock; no daemon migration is authorized.
func handoffExternalInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, legacy, successor inspection.GroupRequestRecord) error {
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
	if successor != privateInspectionGroup(store.RootID, supervisor.Binding()) || legacy.RootID != store.RootID {
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
