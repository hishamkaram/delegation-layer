package infrastructurehost

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Callbacks is immutable native application composition. It carries no launch
// inputs; accepted context owns executable, environment and resident guards.
type Callbacks struct {
	MaintenanceLaunch      func(context.Context, string, string, []string) error
	SuccessfulCleanup      func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error)
	TerminalEvidence       func(context.Context, string, task.SupervisorRef, []pueue.Job) error
	TerminalCustody        func(context.Context, string, task.SupervisorRef, pueue.PrivateTerminalCustodyScope) error
	MigrationAuthorization func(context.Context, string, task.SupervisorRef, []pueue.Job) error
}

func (c Callbacks) ApplyTo(options pueue.Options) pueue.Options {
	if options.PrivateMaintenanceLauncher == nil && options.ControlOwnerExecutable != "" {
		options.PrivateMaintenanceLauncher = c.MaintenanceLaunch
	}
	if options.SuccessfulCleanupAuthorizer == nil {
		options.SuccessfulCleanupAuthorizer = c.SuccessfulCleanup
	}
	if options.PrivateTerminalEvidencePreparer == nil {
		options.PrivateTerminalEvidencePreparer = c.TerminalEvidence
	}
	if options.PrivateTerminalCustodyReleaser == nil {
		options.PrivateTerminalCustodyReleaser = c.TerminalCustody
	}
	if options.PrivateMigrationAuthorizer == nil {
		options.PrivateMigrationAuthorizer = c.MigrationAuthorization
	}
	return options
}
