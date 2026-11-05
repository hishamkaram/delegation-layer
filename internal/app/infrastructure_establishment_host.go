package app

import (
	"context"
	"os"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// infrastructureEstablishmentHost owns an immutable selection snapshot for
// outside artifact preparation. Only fixed application composition constructs
// it; it is not a Dependencies override. Task custom runners and RPC data do
// not select it.
type infrastructureEstablishmentHost struct {
	defaultRunner string
	lookup        supervisor.ExecutableLookup
	initial       infrastructure.InitialOperation
}

var _ infrastructure.EstablishmentHost = infrastructureEstablishmentHost{}

func newInfrastructureEstablishmentHost(defaultRunner string, lookup supervisor.ExecutableLookup, initial infrastructure.InitialOperation) infrastructureEstablishmentHost {
	// Match command-entry capture semantics: nil means not captured yet, whereas
	// nonnil empty is explicit. Later preparation never samples ambient environment.
	if lookup.Environment == nil {
		lookup.Environment = os.Environ()
	}
	lookup.Environment = slices.Clone(lookup.Environment)
	return infrastructureEstablishmentHost{defaultRunner: defaultRunner, lookup: lookup, initial: initial}
}

func (h infrastructureEstablishmentHost) PrepareOutsideArtifacts(ctx context.Context, target *taskdir.Store) (infrastructure.Artifacts, error) {
	if ctx == nil || target == nil {
		return infrastructure.Artifacts{}, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return infrastructure.Artifacts{}, err
	}
	runner, err := supervisor.ResolveRunner("", h.defaultRunner)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	return supervisor.PrepareOutsideInfrastructureArtifacts(ctx, target, runner, h.lookup, h.initial)
}
