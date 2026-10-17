package workspaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func PrepareDispatch(a DispatchRequest, deps OperationOptions, store *taskdir.Store, id, cwd string, brief []byte) (managed *Admission, effectiveCwd string, resultErr error) {
	if a.Isolation == "" && a.WorkspaceID == "" {
		return nil, cwd, RejectUnmanagedPath(cwd)
	}
	if a.RunnerOwnership != task.RunnerOwnershipManaged {
		return nil, "", fmt.Errorf("%w: managed workspaces require the managed delegate-run runner", task.ErrInvalidPermit)
	}
	if err := workspace.ValidateGitEnvironment(); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	budget, err := ParseBudget(a.WorkspaceBudget)
	if err != nil {
		return nil, "", err
	}
	requestDigest, err := DispatchDigest(a, id, cwd, brief, budget, nil)
	if err != nil {
		return nil, "", err
	}
	resource, err := openDispatchResource(ctx, store, a, id, cwd, requestDigest, budget)
	if err != nil {
		return nil, "", err
	}
	managed = &Admission{resource: resource}
	if a.WorkspaceID != "" && a.WorkspaceBudget == "" {
		budget = time.Duration(resource.Request().BudgetNanos)
	}
	requestDigest, err = DispatchDigest(a, id, cwd, brief, budget, resource)
	if err != nil {
		return managed, "", err
	}
	owner, err := resource.AcquireUseOwner(ctx)
	if err != nil {
		return managed, "", err
	}
	managed.owner = owner
	if err = resource.BindAdmission(ctx, id, requestDigest); err != nil {
		return managed, "", err
	}
	if binding, existingCwd, exists, lookupErr := existingDispatch(ctx, store, resource, id, requestDigest); lookupErr != nil || exists {
		managed.binding, managed.existing = binding, exists
		return managed, existingCwd, lookupErr
	}
	effectiveCwd, resultErr = reserveDispatch(ctx, a, deps, store, id, requestDigest, budget, managed)
	return managed, effectiveCwd, resultErr
}

func reserveDispatch(ctx context.Context, a DispatchRequest, deps OperationOptions, store *taskdir.Store, id, requestDigest string, budget time.Duration, managed *Admission) (string, error) {
	resource := managed.resource
	if err := ReconcileUse(ctx, store, resource, deps); err != nil {
		return "", err
	}
	reservation, err := resource.Reserve(ctx, workspace.Subject{Kind: workspace.SubjectAdmission, ID: id, SHA256: requestDigest})
	if err != nil {
		return "", err
	}
	managed.reservation = reservation
	managed.binding = &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: managed.reservation.Sequence, RequestSHA256: requestDigest, BudgetNanos: int64(budget)}
	registration, err := provisionIfNeeded(ctx, a, deps, store, managed, budget)
	if err != nil {
		return "", err
	}
	if _, err = RegisteredExecutionCheckout(ctx, resource); err != nil {
		return "", err
	}
	if a.HasWorkspaceBinding && a.Cwd != "" && a.Cwd != registration.CanonicalCwd {
		return "", task.ErrIdentityMismatch
	}
	return registration.CanonicalCwd, nil
}

func openDispatchResource(ctx context.Context, store *taskdir.Store, a DispatchRequest, id, cwd, requestDigest string, budget time.Duration) (*workspace.Resource, error) {
	if a.WorkspaceID != "" {
		return workspace.LoadResource(ctx, store, a.WorkspaceID)
	}
	workspaceID, err := randomid.NewTaskID()
	if err != nil {
		return nil, err
	}
	return workspace.EstablishResource(ctx, store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, TaskID: id,
		RequestSHA256: requestDigest, SourceCwd: cwd, BaseRef: a.Base,
		Container: workspace.ContainerPath(store.Root, store.RootID), BudgetNanos: int64(budget),
	})
}

func existingDispatch(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, id, requestDigest string) (binding *task.WorkspaceBinding, cwd string, exists bool, resultErr error) {
	td, err := store.OpenTask(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	request, _, err := td.PreparedRecords()
	if err != nil {
		return nil, "", true, err
	}
	if request.Workspace == nil || request.Workspace.ID != resource.Request().WorkspaceID || request.Workspace.RequestSHA256 != requestDigest {
		return nil, "", true, task.ErrIdentityMismatch
	}
	return request.Workspace, request.CanonicalCwd, true, ctx.Err()
}

func (m *Admission) Close() error {
	if m == nil {
		return nil
	}
	var ownerErr error
	if m.owner != nil {
		ownerErr = m.owner.Release()
	}
	return errors.Join(ownerErr, m.resource.Close())
}

// Binding returns a detached admission record, never the owner's mutable field.
func (m *Admission) Binding() *task.WorkspaceBinding {
	if m == nil || m.binding == nil {
		return nil
	}
	binding := *m.binding
	return &binding
}
