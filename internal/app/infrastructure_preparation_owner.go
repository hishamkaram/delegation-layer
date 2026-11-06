package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// One host invocation owns this Store. The entry lends its resident guards;
// canonical consumers own inner resources. An unresolved child retains its Store.
type infrastructurePreparationOwner struct {
	store       *taskdir.Store
	options     pueue.Options
	lookup      supervisor.ExecutableLookup
	observation *infrastructure.PreparationObservation
}

func openInfrastructurePreparation(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (*infrastructurePreparationOwner, error) {
	if ctx == nil {
		return nil, os.ErrInvalid
	}
	if err := operation.Validate(); err != nil {
		return nil, err
	}
	intent, artifacts := operation.Intent(), operation.Artifacts()
	expected, err := infrastructure.NewOrigin(intent.Inputs.Root, intent.Inputs.RootID)
	if err != nil {
		return nil, err
	}
	options, observation, err := infrastructurePreparationOptions(expected, artifacts, selected, guards, wait)
	if err != nil {
		return nil, err
	}
	owner := &infrastructurePreparationOwner{options: options, observation: observation}
	owner.lookup = supervisor.ExecutableLookup{
		InitialExecutable: selected.SupervisorExecutable, Environment: []string{},
		AcceptedBootstrapPair: supervisor.BootstrapPair{Client: selected.SupervisorExecutable, Daemon: filepath.Join(filepath.Dir(selected.SupervisorExecutable), "pueued")},
	}
	owner.store, err = openNativeSupervisorStore(ctx, intent.Inputs.Root)
	if err != nil {
		return owner, err
	}
	pin, err := owner.store.PinRootContext(ctx)
	if err != nil {
		return owner, err
	}
	if pin.Root() != intent.Inputs.Root || pin.RootID() != intent.Inputs.RootID || pin.Device() != intent.Inputs.RootDevice || pin.Inode() != intent.Inputs.RootInode {
		return owner, infrastructure.ErrIdentity
	}
	return owner, nil
}

func (o *infrastructurePreparationOwner) disposition() infrastructure.Disposition {
	if o == nil || o.observation == nil {
		return infrastructure.Disposition{}
	}
	return o.observation.Disposition()
}

func (o *infrastructurePreparationOwner) close(cause error) error {
	if o == nil {
		return cause
	}
	// This runs inside the operation's owned worker, after canonical client
	// closure. Observation expiry never releases its parent resources.
	bootstrapPending, releaseFault, bootstrapErr := collectInfrastructureBootstrapCleanup(cause)
	cause = errors.Join(cause, bootstrapErr)
	if bootstrapPending || cleanupRetainsStore(cause, o.store) {
		return newInfrastructurePreparationCleanupError(o, cause)
	}
	if o.store != nil {
		if err := o.store.Close(); err != nil {
			return newInfrastructurePreparationCleanupError(o, errors.Join(cause, err))
		}
		o.store = nil
	}
	if releaseFault {
		// A retired Go handle is not proof of successful native release. Even
		// without usable handles this fault must stop resident admission.
		return newInfrastructurePreparationCleanupError(o, cause)
	}
	return cause
}

type infrastructurePreparationCleanupError struct {
	owner *infrastructurePreparationOwner
	cause error
	done  <-chan struct{}
}

func newInfrastructurePreparationCleanupError(owner *infrastructurePreparationOwner, cause error) infrastructure.ResidentCleanupCompletion {
	if cause == nil {
		cause = os.ErrInvalid
	}
	done := make(chan struct{})
	close(done)
	return &infrastructurePreparationCleanupError{owner: owner, cause: cause, done: done}
}

func (e *infrastructurePreparationCleanupError) Error() string {
	return "resident preparation cleanup unresolved: " + e.cause.Error()
}

func (e *infrastructurePreparationCleanupError) Unwrap() error { return e.cause }

// RetainsResidentCleanup tells the resident to retain this error's resource
// graph and stop new admission. It is no native closure or launch permission.
func (*infrastructurePreparationCleanupError) RetainsResidentCleanup() {}

func (e *infrastructurePreparationCleanupError) CleanupDone() <-chan struct{} { return e.done }
func (e *infrastructurePreparationCleanupError) CleanupErr() error            { return e.cause }
func (e *infrastructurePreparationCleanupError) CleanupPending() bool {
	return e.owner != nil && e.owner.store != nil
}
