package supervisor

import (
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// captureInfrastructureLaunchContext snapshots the effective options already
// selected by trusted supervisor composition, after its existing provider merge.
// It adds no environment-selection policy. RPC data cannot call this producer
// or become executable/source authority through the resulting record.
func captureInfrastructureLaunchContext(pin taskdir.RootPin, options pueue.Options, selectedClient string) (infrastructure.LaunchContext, error) {
	control := options.Environment
	if control == nil {
		control = pueue.DefaultEnvironment()
	}
	maintenance := options.PrivateMaintenanceEnvironment
	if maintenance == nil {
		maintenance = pueue.DefaultEnvironment()
	}
	// Incoming evidence must be extracted by composition before this overlay;
	// removal and rebasing do not authorize an outside owner.
	origin, err := infrastructure.NewOrigin(pin.Root(), pin.RootID())
	if err != nil {
		return infrastructure.LaunchContext{}, err
	}
	control, err = infrastructure.WithOrigin(lifetime.WithoutProcessScopeMarker(control), origin)
	if err != nil {
		return infrastructure.LaunchContext{}, err
	}
	maintenance, err = infrastructure.WithOrigin(lifetime.WithoutProcessScopeMarker(maintenance), origin)
	if err != nil {
		return infrastructure.LaunchContext{}, err
	}
	record := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: pin.Root(), RootID: pin.RootID(), OS: runtime.GOOS, SupervisorExecutable: selectedClient,
		ControlEnvironment: control, MaintenanceEnvironment: maintenance,
	}
	if _, err = record.Digest(); err != nil {
		return infrastructure.LaunchContext{}, err
	}
	return record, nil
}

// Routed work selects its target's recorded installed client first. This is
// metadata and path selection only; the resident re-reads canonical authority.
func (r ControlRuntime) captureLaunchContext(pin taskdir.RootPin) (infrastructure.LaunchContext, error) {
	saved, found, err := pueue.ReadPrivateSupervisorBinding(pin.Root())
	if err != nil {
		return infrastructure.LaunchContext{}, err
	}
	var client string
	if found {
		client, err = ResolveSavedSupervisorClient(pin.Root(), saved, r.Lookup)
	} else {
		client, err = resolveInstalledSupervisorClient(r.Lookup)
	}
	if err != nil {
		return infrastructure.LaunchContext{}, err
	}
	return captureInfrastructureLaunchContext(pin, r.Options, client)
}
