package app

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// infrastructureSupervisorOptions composes previously validated accepted data.
// The journal owns the snapshots; these options own independent environment copies.
// The fixed entry validates native custody and context before calling this helper
// and separately supplies its private daemon-start grant afterward.
func infrastructureSupervisorOptions(artifacts infrastructure.Artifacts, launch infrastructure.LaunchContext) pueue.Options {
	return withDefaultSupervisorCallbacks(pueue.Options{
		ControlOwnerExecutable:        artifacts.Runner,
		Environment:                   slices.Clone(launch.ControlEnvironment),
		PrivateMaintenanceEnvironment: slices.Clone(launch.MaintenanceEnvironment),
	})
}

// withDefaultSupervisorCallbacks shares app policy composition without discovery.
func withDefaultSupervisorCallbacks(options pueue.Options) pueue.Options {
	if options.PrivateMaintenanceLauncher == nil && options.ControlOwnerExecutable != "" {
		options.PrivateMaintenanceLauncher = supervisor.LaunchQueueMaintenanceOwnerWithEnvironment
	}
	if options.SuccessfulCleanupAuthorizer == nil {
		options.SuccessfulCleanupAuthorizer = authorizePrivateSuccessfulCleanup
	}
	if options.PrivateTerminalEvidencePreparer == nil {
		options.PrivateTerminalEvidencePreparer = preparePrivateTerminalEvidence
	}
	if options.PrivateTerminalCustodyReleaser == nil {
		options.PrivateTerminalCustodyReleaser = releasePrivateTerminalCustody
	}
	if options.PrivateMigrationAuthorizer == nil {
		options.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	}
	return options
}
