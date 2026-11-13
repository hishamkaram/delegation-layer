package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// RuntimeRestartRouting is the explicit restart capability. Ordinary admission
// routing need not implement it. The target journal remains the state owner.
type RuntimeRestartRouting interface {
	RestartOwn(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error)
}

// RestartOwn borrows store and routes the exact original startup once. Contact
// uncertainty never authorizes local recovery, another root, or another launch.
func (r ControlRuntime) RestartOwn(ctx context.Context, store *taskdir.Store, from infrastructure.RuntimeRestartReference) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
	if ctx == nil || store == nil {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, os.ErrInvalid
	}
	if err := from.Validate(); err != nil {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, err
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, err
	}
	if pin.RootID() != from.RootID {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, task.ErrIdentityMismatch
	}
	routing, ok := r.Routing.(RuntimeRestartRouting)
	if !ok {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, ErrInfrastructureAuthorityUnavailable
	}
	selected, err := r.captureLaunchContext(pin)
	if err != nil {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, err
	}
	disposition, report, contactErr := routing.RestartOwn(operation, store, selected, from)
	err = errors.Join(contactErr, checkInfrastructureTarget(operation, store, pin))
	if report == (infrastructure.RuntimeRestartReport{}) {
		if err == nil {
			err = infrastructure.ErrIdentity
		}
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, err
	}
	if report.From != from.Startup {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, errors.Join(err, infrastructure.ErrIdentity)
	}
	if disposition != (infrastructure.Disposition{}) {
		if invalid := disposition.Validate(); invalid != nil {
			return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, errors.Join(err, invalid)
		}
	}
	if invalid := report.ValidateDisposition(disposition); invalid != nil {
		return infrastructure.Disposition{}, infrastructure.RuntimeRestartReport{}, errors.Join(err, invalid)
	}
	return disposition, report, err
}
