package app

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The fixed host owns source lookup; caller data supplies no executable paths.
// Observation performs no target preparation and grants no native Start.
func (h infrastructureEstablishmentHost) outsideRequirements(ctx context.Context, target *taskdir.Store) (infrastructure.ArtifactRequirements, error) {
	if ctx == nil || target == nil {
		return infrastructure.ArtifactRequirements{}, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	runner, err := supervisor.ResolveRunner("", h.defaultRunner)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	return supervisor.ObserveOutsideInfrastructureRequirements(ctx, target, runner, h.lookup, h.initial)
}

type infrastructureRoutingRequirements struct {
	requirements infrastructure.ArtifactRequirements
	owner        infrastructure.StartupReference
	probeErr     error
}

// Only authenticated success selects an existing epoch. Every other status
// observation leaves birth eligibility to the existing native election path.
func (h infrastructureEstablishmentHost) preparationRequirements(ctx context.Context, target *taskdir.Store, reader *taskdir.ReadOnlyStore) (selected infrastructureRoutingRequirements, resultErr error) {
	if h.initial.Kind != infrastructure.InitialMigrateOwn {
		owner, pair, probeErr := infrastructure.ProbeOwnerArtifacts(ctx, reader)
		if probeErr == nil {
			selected.owner = owner
			selected.requirements, resultErr = h.ownerRequirements(ctx, target, pair)
			return selected, resultErr
		}
		if infrastructureRoutingRetainsCleanup(probeErr) {
			return selected, probeErr
		}
		selected.probeErr = probeErr
	}
	selected.requirements, resultErr = h.outsideRequirements(ctx, target)
	return selected, resultErr
}

func (h infrastructureEstablishmentHost) ownerRequirements(ctx context.Context, target *taskdir.Store, pair infrastructure.OwnerEpochPair) (infrastructure.ArtifactRequirements, error) {
	if err := pair.Validate(); err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	runner, err := supervisor.ResolveRunner("", h.defaultRunner)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	runtimeSHA256, err := supervisor.ObserveOutsideInfrastructureRuntime(ctx, target, runner)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	return infrastructure.ArtifactRequirements{RequiredRuntimeSHA256: runtimeSHA256, RequiredClientSHA256: pair.ClientSHA256, RequiredDaemonSHA256: pair.DaemonSHA256}, nil
}
