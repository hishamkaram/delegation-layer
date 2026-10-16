package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type managedWorkspaceAdmission struct {
	resource    *workspace.Resource
	owner       *taskdir.LifetimeOwnerLease
	reservation workspace.Reservation
	binding     *task.WorkspaceBinding
	operationID string
	existing    bool
}

type managedDispatchIdentity struct {
	TaskID               string          `json:"task_id"`
	Provider             string          `json:"provider"`
	Auto                 bool            `json:"auto"`
	SourceCwd            string          `json:"source_cwd"`
	WorkspaceID          string          `json:"workspace_id,omitempty"`
	Base                 string          `json:"base"`
	Config               task.TaskConfig `json:"config"`
	BriefSHA256          string          `json:"brief_sha256"`
	WorkspaceBudgetNanos int64           `json:"workspace_budget_nanos"`
}

func prepareManagedDispatch(a Arguments, deps Dependencies, store *taskdir.Store, id, cwd string, brief []byte) (managed *managedWorkspaceAdmission, effectiveCwd string, resultErr error) {
	if a.Isolation == "" && a.WorkspaceID == "" {
		return nil, cwd, rejectUnmanagedWorkspacePath(cwd)
	}
	if requestedRunnerOwnership(a) != task.RunnerOwnershipManaged {
		return nil, "", fmt.Errorf("%w: managed workspaces require the managed delegate-run runner", task.ErrInvalidPermit)
	}
	if err := workspace.ValidateGitEnvironment(); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	budget, err := parseWorkspaceBudget(a.WorkspaceBudget)
	if err != nil {
		return nil, "", err
	}
	requestDigest, err := managedDispatchDigest(a, id, cwd, brief, budget, nil)
	if err != nil {
		return nil, "", err
	}
	resource, err := openManagedDispatchResource(ctx, store, a, id, cwd, requestDigest, budget)
	if err != nil {
		return nil, "", err
	}
	managed = &managedWorkspaceAdmission{resource: resource}
	if a.WorkspaceID != "" && a.WorkspaceBudget == "" {
		budget = time.Duration(resource.Request().BudgetNanos)
	}
	requestDigest, err = managedDispatchDigest(a, id, cwd, brief, budget, resource)
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
	if binding, existingCwd, exists, lookupErr := existingManagedDispatch(ctx, store, resource, id, requestDigest); lookupErr != nil || exists {
		managed.binding, managed.existing = binding, exists
		return managed, existingCwd, lookupErr
	}
	effectiveCwd, resultErr = reserveManagedDispatch(ctx, a, deps, store, id, requestDigest, budget, managed)
	return managed, effectiveCwd, resultErr
}

func managedDispatchDigest(a Arguments, id, cwd string, brief []byte, budget time.Duration, resource *workspace.Resource) (string, error) {
	identity := managedDispatchIdentity{TaskID: id, Provider: a.Provider, Auto: a.Auto, SourceCwd: cwd, WorkspaceID: a.WorkspaceID, Base: a.Base, Config: a.Config, BriefSHA256: task.ComputeSHA256(brief), WorkspaceBudgetNanos: int64(budget)}
	if resource != nil && a.WorkspaceID != "" {
		identity.SourceCwd = ""
		initial := resource.Request()
		if initial.TaskID == id {
			identity.SourceCwd, identity.Base, identity.WorkspaceID = initial.SourceCwd, initial.BaseRef, ""
		}
	}
	data, err := task.MarshalCanonical(identity)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func reserveManagedDispatch(ctx context.Context, a Arguments, deps Dependencies, store *taskdir.Store, id, requestDigest string, budget time.Duration, managed *managedWorkspaceAdmission) (string, error) {
	resource := managed.resource
	if err := reconcileWorkspaceUse(ctx, store, resource, deps); err != nil {
		return "", err
	}
	reservation, err := resource.Reserve(ctx, workspace.Subject{Kind: workspace.SubjectAdmission, ID: id, SHA256: requestDigest})
	if err != nil {
		return "", err
	}
	managed.reservation = reservation
	managed.binding = &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: managed.reservation.Sequence, RequestSHA256: requestDigest, BudgetNanos: int64(budget)}
	registration, err := provisionWorkspaceIfNeeded(ctx, a, deps, store, managed, budget)
	if err != nil {
		return "", err
	}
	if _, err = registeredExecutionCheckout(ctx, resource); err != nil {
		return "", err
	}
	if a.workspaceBinding != nil && a.Cwd != "" && a.Cwd != registration.CanonicalCwd {
		return "", task.ErrIdentityMismatch
	}
	return registration.CanonicalCwd, nil
}

func provisionWorkspaceIfNeeded(ctx context.Context, _ Arguments, deps Dependencies, store *taskdir.Store, managed *managedWorkspaceAdmission, budget time.Duration) (workspace.Registration, error) {
	resource := managed.resource
	registration, registrationErr := resource.Registration(ctx)
	if registrationErr != nil && !workspace.OperationAbsent(registrationErr) {
		return registration, registrationErr
	}
	attempt, err := resource.ResolveAdmissionAttempt(ctx, managed.reservation.Sequence, managed.reservation.Subject)
	if err != nil {
		return registration, err
	}
	purpose, err := attempt.ProvisionPurpose()
	if err != nil {
		return registration, err
	}
	_, purposeErr := resource.AssociatedOperation(ctx, purpose)
	if purposeErr != nil && !workspace.OperationAbsent(purposeErr) {
		return registration, purposeErr
	}
	if registrationErr == nil && workspace.OperationAbsent(purposeErr) {
		return registration, requireWorkspaceProvisionHelpersClosed(ctx, store, resource)
	}
	operation, err := scheduleWorkspaceOperation(ctx, store, resource, deps, workspaceOperationSpec{
		purpose: purpose, kind: workspace.OperationProvision, subject: managed.reservation.Subject, budget: budget,
	}, nil)
	if operation != nil {
		managed.operationID = operation.Request().OperationID
	}
	if err = errors.Join(err, closeWorkspaceOperation(operation)); err != nil {
		return registration, err
	}
	if err = ctx.Err(); err != nil {
		return registration, errors.Join(workspace.ErrPending, err)
	}
	return awaitWorkspaceProvision(ctx, store, resource, managed.operationID)
}

// Registration visibility is not process closure. A reused checkout cannot
// admit a provider while any provision helper still owns its native lifetime.
func requireWorkspaceProvisionHelpersClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	ids, err := resource.OperationIDs(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, id := range ids {
		operation, loadErr := workspace.LoadOperation(ctx, store, id)
		if loadErr != nil {
			return loadErr
		}
		var proofErr error
		if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
			proofErr = task.ErrIdentityMismatch
		} else if operation.Request().Kind == workspace.OperationProvision {
			found = true
			_, proofErr = workspaceOperationClosed(ctx, operation)
		}
		if err = errors.Join(proofErr, operation.Close()); err != nil {
			return err
		}
	}
	if !found {
		return task.ErrEvidenceFault
	}
	return nil
}

func openManagedDispatchResource(ctx context.Context, store *taskdir.Store, a Arguments, id, cwd, requestDigest string, budget time.Duration) (*workspace.Resource, error) {
	if a.WorkspaceID != "" {
		return workspace.LoadResource(ctx, store, a.WorkspaceID)
	}
	workspaceID, err := task.NewTaskID()
	if err != nil {
		return nil, err
	}
	return workspace.EstablishResource(ctx, store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, TaskID: id,
		RequestSHA256: requestDigest, SourceCwd: cwd, BaseRef: a.Base,
		Container: workspace.ContainerPath(store.Root, store.RootID), BudgetNanos: int64(budget),
	})
}

func existingManagedDispatch(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, id, requestDigest string) (binding *task.WorkspaceBinding, cwd string, exists bool, resultErr error) {
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

func awaitWorkspaceProvision(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string) (registration workspace.Registration, resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return registration, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	for {
		_, _, err = acceptedWorkspaceOperation(ctx, store, operation)
		if err == nil {
			return resource.Registration(ctx)
		}
		if !errors.Is(err, workspace.ErrPending) {
			return registration, err
		}
		if err = waitWorkspaceObservation(ctx); err != nil {
			return registration, errors.Join(workspace.ErrPending, err)
		}
	}
}

func waitWorkspaceObservation(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *managedWorkspaceAdmission) Close() error {
	if m == nil {
		return nil
	}
	var ownerErr error
	if m.owner != nil {
		ownerErr = m.owner.Release()
	}
	return errors.Join(ownerErr, m.resource.Close())
}

func rejectUnmanagedWorkspacePath(cwd string) error {
	for path := cwd; path != filepath.Dir(path); path = filepath.Dir(path) {
		marker := filepath.Join(path, ".delegate-workspace")
		info, err := os.Lstat(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed workspace requires --workspace ID with its owning state root")
		}
	}
	return nil
}
