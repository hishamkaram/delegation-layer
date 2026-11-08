package infrastructurehost

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// infrastructureSupervisorOptions composes previously validated accepted data.
// The journal owns the snapshots; these options own independent environment copies.
// The fixed entry validates native custody and context before calling this helper
// and separately supplies its private daemon-start grant afterward.
func infrastructureSupervisorOptions(artifacts infrastructure.Artifacts, launch infrastructure.LaunchContext, callbacks Callbacks) pueue.Options {
	return callbacks.ApplyTo(pueue.Options{
		RuntimeClientExecutable:       launch.SupervisorExecutable,
		ControlOwnerExecutable:        artifacts.Runner,
		PrivateTerminalEventRunner:    artifacts.Runner,
		Environment:                   slices.Clone(launch.ControlEnvironment),
		PrivateMaintenanceEnvironment: slices.Clone(launch.MaintenanceEnvironment),
	})
}
