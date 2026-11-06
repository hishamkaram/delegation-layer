package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type infrastructureOwnerObservation struct {
	owner    infrastructure.StartupReference
	probeErr error
}

// Authenticated reuse does not depend on installation equality. Probe failures
// remain diagnostics unless they retain cleanup; election owns birth permission.
func (h infrastructureEstablishmentHost) probePreparationOwner(ctx context.Context, reader *taskdir.ReadOnlyStore) (selected infrastructureOwnerObservation, resultErr error) {
	if h.initial.Kind == infrastructure.InitialMigrateOwn {
		return selected, nil
	}
	owner, probeErr := infrastructure.ProbeOwner(ctx, reader)
	if probeErr == nil {
		selected.owner = owner
		return selected, nil
	}
	if infrastructureRoutingRetainsCleanup(probeErr) {
		return selected, probeErr
	}
	selected.probeErr = probeErr
	return selected, nil
}
