package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ControlClientOpener opens the exact saved endpoint for a task stop. The
// supervisor owner implements endpoint recovery; this workflow owns the client
// until its control command and retained leases finish.
type ControlClientOpener func(context.Context, string, task.SupervisorRef, pueue.Options, string) (*pueue.Client, error)
