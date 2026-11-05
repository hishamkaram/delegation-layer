package workspaces

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func PrepareDispatch(a DispatchRequest, deps OperationOptions, store *taskdir.Store, id, cwd string, brief []byte) (*Admission, string, error) {
	return PrepareDispatchContext(context.Background(), a, deps, store, id, cwd, brief)
}

// PrepareDispatchContext holds root admission before opening a resource. A
// returned Admission owns any new lease, including on a later preparation
// error, and keeps it until FinishContext or Close completes.
func PrepareDispatchContext(parent context.Context, a DispatchRequest, deps OperationOptions, store *taskdir.Store, id, cwd string, brief []byte) (managed *Admission, effectiveCwd string, resultErr error) {
	if a.Isolation == "" && a.WorkspaceID == "" {
		return nil, cwd, RejectUnmanagedPath(cwd)
	}
	if err := RequireManagedRunner(a); err != nil {
		return nil, "", err
	}
	if err := deps.ValidateGitEnvironment(); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(parent, workspace.DefaultBudget)
	defer cancel()
	budget, err := ParseBudget(a.WorkspaceBudget)
	if err != nil {
		return nil, "", err
	}
	requestDigest, err := DispatchDigest(a, id, cwd, brief, budget, nil)
	if err != nil {
		return nil, "", err
	}
	lease, err := acquireWorkspaceRootAdmission(ctx, store, deps)
	if lease != nil && lease != deps.RootAdmission {
		managed = &Admission{rootAdmission: lease}
	}
	if err != nil {
		return managed, "", err
	}
	deps.RootAdmission = lease
	resource, err := openDispatchResource(ctx, store, a, id, cwd, requestDigest, budget)
	if err != nil {
		return managed, "", err
	}
	if managed == nil {
		managed = &Admission{}
	}
	managed.resource = resource
	effectiveCwd, resultErr = prepareDispatchUse(ctx, a, deps, store, id, cwd, brief, budget, managed)
	return managed, effectiveCwd, resultErr
}

// prepareDispatchUse owns use-owner acquisition, immutable binding and
// reservation preparation under the already held outer root admission.
func prepareDispatchUse(ctx context.Context, a DispatchRequest, deps OperationOptions, store *taskdir.Store, id, cwd string, brief []byte, budget time.Duration, managed *Admission) (string, error) {
	if a.WorkspaceID != "" && a.WorkspaceBudget == "" {
		budget = time.Duration(managed.resource.Request().BudgetNanos)
	}
	requestDigest, err := DispatchDigest(a, id, cwd, brief, budget, managed.resource)
	if err != nil {
		return "", err
	}
	owner, err := managed.resource.AcquireUseOwner(ctx)
	managed.owner = owner
	if err != nil {
		return "", err
	}
	if err = managed.resource.BindAdmission(ctx, id, requestDigest); err != nil {
		return "", err
	}
	if binding, existingCwd, exists, lookupErr := existingDispatch(ctx, store, managed.resource, id, requestDigest); lookupErr != nil || exists {
		managed.binding, managed.existing = binding, exists
		return existingCwd, lookupErr
	}
	return reserveDispatch(ctx, a, deps, store, id, requestDigest, budget, managed)
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

func (m *Admission) Close() (resultErr error) {
	if m == nil {
		return nil
	}
	defer func() {
		if resultErr != nil {
			resultErr = &AdmissionCleanupError{admission: m, cause: resultErr}
		}
	}()
	if m.storeCleanupPending() {
		return m.storeCleanup
	}
	m.storeCleanup = nil
	for len(m.childOwners) != 0 {
		if err := m.childOwners[0].ReleaseWithError(nil); err != nil {
			return err
		}
		m.childOwners[0] = nil
		m.childOwners = m.childOwners[1:]
	}
	if m.owner != nil {
		if err := m.owner.ReleaseWithError(nil); err != nil {
			return err
		}
		m.owner = nil
	}
	if m.resource != nil {
		if err := m.resource.Close(); err != nil {
			return err
		}
		m.resource = nil
	}
	if m.rootAdmission != nil {
		if err := m.rootAdmission.Release(); err != nil {
			return err
		}
		m.rootAdmission = nil
	}
	return nil
}

// Binding returns a detached admission record, never the owner's mutable field.
func (m *Admission) Binding() *task.WorkspaceBinding {
	if m == nil || m.binding == nil {
		return nil
	}
	binding := *m.binding
	return &binding
}
