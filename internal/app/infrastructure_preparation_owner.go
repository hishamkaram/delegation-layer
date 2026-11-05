package app

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// One host invocation owns this Store and its outer admission lease. The entry
// lends its resident guards; canonical consumers own their inner resources.
// A failed inner cleanup keeps both outer resources reachable from the error.
type infrastructurePreparationOwner struct {
	store       *taskdir.Store
	admission   *taskdir.RootAdmissionLease
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
		InitialExecutable: artifacts.Client, Environment: []string{},
		AcceptedBootstrapPair: supervisor.BootstrapPair{Client: artifacts.Client, Daemon: artifacts.Daemon},
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
	owner.admission, err = supervisor.AcquireOrBorrowRootAdmissionContext(ctx, owner.store, nil, pueue.PrivateControlTimeout(options))
	return owner, err
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
	cause = supervisor.ReleaseOwnedRootAdmission(o.admission, cause)
	if o.admission.RetainsStore(o.store) {
		return &infrastructurePreparationCleanupError{owner: o, cause: cause}
	}
	if o.store != nil {
		if err := o.store.Close(); err != nil {
			return &infrastructurePreparationCleanupError{owner: o, cause: errors.Join(cause, err)}
		}
	}
	return cause
}

type infrastructurePreparationCleanupError struct {
	owner *infrastructurePreparationOwner
	cause error
}

func (e *infrastructurePreparationCleanupError) Error() string {
	return "resident preparation cleanup unresolved: " + e.cause.Error()
}

func (e *infrastructurePreparationCleanupError) Unwrap() error { return e.cause }

// RetainsResidentCleanup tells the resident to retain this error's resource
// graph and stop new admission. It is no native closure or launch permission.
func (*infrastructurePreparationCleanupError) RetainsResidentCleanup() {}
