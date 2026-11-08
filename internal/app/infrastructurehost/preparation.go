package infrastructurehost

import (
	"os"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// The request handler owns context resolution. This host edge validates its
// selected snapshot and creates detached options; it never reads an epoch
// default. The observation reports canonical readiness/Start, not authority.
func infrastructurePreparationOptions(expected infrastructure.Origin, artifacts infrastructure.Artifacts, selected infrastructure.LaunchContext, guard infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner, callbacks Callbacks) (pueue.Options, *infrastructure.PreparationObservation, error) {
	if guard == nil || wait == nil {
		return pueue.Options{}, nil, os.ErrInvalid
	}
	if _, err := infrastructure.NewOrigin(expected.Root(), expected.RootID()); err != nil {
		return pueue.Options{}, nil, err
	}
	selected = selected.Clone()
	if err := selected.Validate(); err != nil {
		return pueue.Options{}, nil, err
	}
	if selected.Root != expected.Root() || selected.RootID != expected.RootID() || selected.OS != runtime.GOOS {
		return pueue.Options{}, nil, infrastructure.ErrIdentity
	}
	digest, err := selected.Digest()
	if err != nil {
		return pueue.Options{}, nil, err
	}
	observation, err := infrastructure.NewPreparationObservation(guard, infrastructure.LaunchContextReference{SHA256: digest})
	if err != nil {
		return pueue.Options{}, nil, err
	}
	options := infrastructureSupervisorOptions(artifacts, selected, callbacks)
	options.DaemonStartGuard = observation
	options.PrivateGenerationGuard = guard
	options.DaemonWaitOwner = wait
	options.WaitForPendingClose = true
	return options, observation, nil
}
