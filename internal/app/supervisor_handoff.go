package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
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
