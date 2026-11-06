package app

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// infrastructureEstablishmentHost owns an immutable selection snapshot for
// outside installed-runner selection. Only fixed application composition constructs
// it; it is not a Dependencies override. Task custom runners and RPC data do
// not select it.
type infrastructureEstablishmentHost struct {
	defaultRunner string
	initial       infrastructure.InitialOperation
}

var _ infrastructure.EstablishmentHost = infrastructureEstablishmentHost{}

func newInfrastructureEstablishmentHost(defaultRunner string, initial infrastructure.InitialOperation) infrastructureEstablishmentHost {
	return infrastructureEstablishmentHost{defaultRunner: defaultRunner, initial: initial}
}

func (h infrastructureEstablishmentHost) PrepareOutsideRunner(ctx context.Context, target *taskdir.Store) (string, error) {
	if ctx == nil || target == nil {
		return "", os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return supervisor.ResolveRunner("", h.defaultRunner)
}
