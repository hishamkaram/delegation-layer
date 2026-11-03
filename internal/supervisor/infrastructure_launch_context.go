package supervisor

import (
	"runtime"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// captureInfrastructureLaunchContext snapshots the effective options already
// selected by trusted supervisor composition, after its existing provider merge.
// It adds no environment-selection policy. RPC data cannot call this producer
// or become executable/source authority through the resulting record.
func captureInfrastructureLaunchContext(pin taskdir.RootPin, options pueue.Options) (infrastructure.LaunchContext, error) {
	control := options.Environment
	if control == nil {
		control = pueue.DefaultEnvironment()
	}
	maintenance := options.PrivateMaintenanceEnvironment
	if maintenance == nil {
		maintenance = pueue.DefaultEnvironment()
	}
	record := infrastructure.LaunchContext{
		Protocol: infrastructure.ProtocolVersion, Root: pin.Root(), RootID: pin.RootID(), OS: runtime.GOOS,
		ControlEnvironment: slices.Clone(control), MaintenanceEnvironment: slices.Clone(maintenance),
	}
	if _, err := record.Digest(); err != nil {
		return infrastructure.LaunchContext{}, err
	}
	return record, nil
}
