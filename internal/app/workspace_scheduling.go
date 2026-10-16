package app

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type workspaceBeforeSubmit func(context.Context, *workspace.Operation) error

type workspaceControlDeadlineKey struct{}

// The coordinator owns the complete scheduling transaction. A short observer
// cannot interrupt its receipt-before-enqueue commit; neither observation nor
// replay extends the helper's original deadline or this control cutoff.
func scheduleWorkspaceOperation(observer context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, spec workspaceOperationSpec, beforeSubmit workspaceBeforeSubmit) (*workspace.Operation, error) {
	control, cancel := workspaceSchedulingControl(observer, deps)
	defer cancel()
	deadline, err := workspaceOperationDeadline(spec)
	if err != nil {
		return nil, err
	}
	operation, err := resolveWorkspaceSchedulingPurpose(control, store, resource, spec)
	if err != nil {
		return nil, err
	}
	if operation != nil {
		return operation, scheduleResolvedWorkspaceOperation(control, store, resource, operation, deps, spec, beforeSubmit)
	}
	spec.deadline = deadline
	parsed, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		return nil, err
	}
	// Expired, never-created purposes still need their immutable request and
	// no-launch fence. This control-only path never obtains a submission permit.
	if !time.Now().Before(parsed) {
		operation, err = ensureWorkspaceOperation(control, store, resource, deps, spec)
		if err != nil {
			return operation, err
		}
		_, err = workspaceOperationClosed(control, operation)
		return operation, err
	}
	scheduling, finish := context.WithDeadline(control, parsed)
	defer finish()
	operation, err = ensureWorkspaceOperation(scheduling, store, resource, deps, spec)
	if err != nil {
		return operation, err
	}
	return operation, submitWorkspaceOperationAfter(scheduling, store, operation, deps, beforeSubmit)
}

func workspaceSchedulingControl(observer context.Context, deps Dependencies) (context.Context, context.CancelFunc) {
	// Only this owner stores the immutable cutoff. Nested scheduling/finalization
	// keeps its parent's cancellation and deadline instead of detaching again.
	if cutoff, ok := observer.Value(workspaceControlDeadlineKey{}).(time.Time); ok {
		return context.WithDeadline(observer, cutoff)
	}
	cutoff := time.Now().Add(pueue.PrivateControlTimeout(deps.SupervisorOptions))
	control, cancel := context.WithDeadline(context.WithoutCancel(observer), cutoff)
	return context.WithValue(control, workspaceControlDeadlineKey{}, cutoff), cancel
}

func resolveWorkspaceSchedulingPurpose(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, spec workspaceOperationSpec) (*workspace.Operation, error) {
	reference, err := resource.AssociatedOperation(ctx, spec.purpose)
	if workspace.OperationAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readWorkspacePurpose(ctx, store, resource, reference, spec)
}

func scheduleResolvedWorkspaceOperation(control context.Context, store *taskdir.Store, resource *workspace.Resource, operation *workspace.Operation, deps Dependencies, spec workspaceOperationSpec, beforeSubmit workspaceBeforeSubmit) error {
	if !time.Now().Before(operation.Deadline()) {
		if err := bindWorkspacePurpose(control, resource, operation.Request(), spec); err != nil {
			return err
		}
		_, err := workspaceOperationClosed(control, operation)
		return err
	}
	scheduling, cancel := context.WithDeadline(control, operation.Deadline())
	defer cancel()
	if err := bindWorkspacePurpose(scheduling, resource, operation.Request(), spec); err != nil {
		return err
	}
	return submitWorkspaceOperationAfter(scheduling, store, operation, deps, beforeSubmit)
}

func submitWorkspaceOperationAfter(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, deps Dependencies, beforeSubmit workspaceBeforeSubmit) error {
	if beforeSubmit != nil {
		if err := beforeSubmit(ctx, operation); err != nil {
			return err
		}
	}
	return submitWorkspaceOperation(ctx, store, operation, deps)
}

// Redrive keeps the same request, submission permit, and absolute cutoff.
func scheduleExistingWorkspaceOperation(observer context.Context, store *taskdir.Store, operation *workspace.Operation, deps Dependencies) error {
	control, cancel := workspaceSchedulingControl(observer, deps)
	defer cancel()
	if !time.Now().Before(operation.Deadline()) {
		_, err := workspaceOperationClosed(control, operation)
		return err
	}
	scheduling, finish := context.WithDeadline(control, operation.Deadline())
	defer finish()
	return submitWorkspaceOperation(scheduling, store, operation, deps)
}

func closeWorkspaceOperation(operation *workspace.Operation) error {
	if operation == nil {
		return nil
	}
	return operation.Close()
}
