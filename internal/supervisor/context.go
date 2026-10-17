package supervisor

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

const supervisorMigrationTimeout = 30 * time.Second

// ControlContext applies the existing finite supervisor control timeout.
func ControlContext(options pueue.Options) (context.Context, context.CancelFunc) {
	return ControlContextWithParent(context.Background(), options)
}

// ControlContextWithParent bounds control work without replacing caller cancellation.
func ControlContextWithParent(parent context.Context, options pueue.Options) (context.Context, context.CancelFunc) {
	timeout := pueue.PrivateControlTimeout(options)
	if timeout <= 0 {
		timeout = supervisorMigrationTimeout
	}
	return context.WithTimeout(parent, timeout)
}
