package infrastructurehost

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// The referenced journal owns the binding. Fixed owner composition re-reads
// that exact slot through its canonical reader; the source selects no path or
// executable. The caller owns the borrowed Store throughout this resolution.
func resolveInfrastructureSavedBinding(ctx context.Context, store *taskdir.Store, source infrastructure.SavedBindingSource) (task.SupervisorRef, error) {
	if source.Kind == infrastructure.SavedBindingTaskMetadata {
		return supervisor.ReadTaskMetadataSupervisor(ctx, store, source)
	}
	if ctx == nil || store == nil {
		return task.SupervisorRef{}, os.ErrInvalid
	}
	if err := source.Validate(); err != nil {
		return task.SupervisorRef{}, err
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if source.RootID != pin.RootID() {
		return task.SupervisorRef{}, infrastructure.ErrIdentity
	}
	binding, err := readInfrastructureSavedBinding(ctx, store, source)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if err = source.ValidateBinding(binding); err != nil {
		return task.SupervisorRef{}, err
	}
	if !pueue.IsPrivateConfig(pin.Root(), binding.ConfigPath) {
		return task.SupervisorRef{}, infrastructure.ErrIdentity
	}
	current, err := store.PinRootContext(ctx)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if !pin.Equal(current) {
		return task.SupervisorRef{}, infrastructure.ErrIdentity
	}
	return binding, nil
}

func readInfrastructureSavedBinding(ctx context.Context, store *taskdir.Store, source infrastructure.SavedBindingSource) (binding task.SupervisorRef, resultErr error) {
	if source.Kind == infrastructure.SavedBindingWorkspaceRequest {
		operation, err := workspace.LoadOperation(ctx, store, source.OwnerID)
		if err != nil {
			return binding, err
		}
		defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
		return operation.Request().Supervisor, nil
	}
	td, err := store.OpenTask(source.OwnerID)
	if err != nil {
		return binding, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	switch source.Kind {
	case infrastructure.SavedBindingTaskMetadata:
		return binding, infrastructure.ErrIdentity
	case infrastructure.SavedBindingTaskSubmission:
		submission, readErr := td.ReadSubmission()
		if readErr != nil {
			return binding, readErr
		}
		return submission.Supervisor, nil
	case infrastructure.SavedBindingTaskStop:
		request, readErr := td.ReadStopRequest(source.RecordID)
		if readErr != nil {
			return binding, readErr
		}
		return request.Supervisor, nil
	case infrastructure.SavedBindingWorkspaceRequest:
		return binding, infrastructure.ErrIdentity
	default:
		return binding, infrastructure.ErrIdentity
	}
}
