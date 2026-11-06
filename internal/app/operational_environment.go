package app

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The borrowed Store owns the root identity. This overlay publishes only its
// current locator and never interprets an incoming locator as placement proof.
// The selected environment already belongs to the finite launch's scope.
func operationalEnvironment(ctx context.Context, store *taskdir.Store, selected []string) ([]string, error) {
	if store == nil {
		return nil, os.ErrInvalid
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	origin, err := infrastructure.NewOrigin(pin.Root(), pin.RootID())
	if err != nil {
		return nil, err
	}
	return infrastructure.WithOrigin(selected, origin)
}

// The invocation retains the borrowed Store until every owned command joins.
// Each callback refreshes its native root observation at the final launch edge.
func operationalEnvironmentPreparation(store *taskdir.Store) func(context.Context, []string) ([]string, error) {
	return func(ctx context.Context, selected []string) ([]string, error) {
		return operationalEnvironment(ctx, store, selected)
	}
}
