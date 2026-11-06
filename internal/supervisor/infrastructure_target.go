package supervisor

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func checkInfrastructureTarget(ctx context.Context, target *taskdir.Store, expected taskdir.RootPin) error {
	if target == nil {
		return os.ErrInvalid
	}
	current, err := target.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return infrastructure.ErrIdentity
	}
	return nil
}
