package workspaces

import (
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func DispatchDigest(a DispatchRequest, id, cwd string, brief []byte, budget time.Duration, resource *workspace.Resource) (string, error) {
	identity := dispatchIdentity{TaskID: id, Provider: a.Provider, Auto: a.Auto, SourceCwd: cwd, WorkspaceID: a.WorkspaceID, Base: a.Base, Config: a.Config, BriefSHA256: task.ComputeSHA256(brief), WorkspaceBudgetNanos: int64(budget)}
	if a.Team != nil && a.TeamIntent != nil {
		target := a.Team.Clone()
		identity.Team = &target
		identity.Provider, identity.Auto, identity.Config = a.TeamIntent.Provider, a.TeamIntent.Auto, a.TeamIntent.Config
	}
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

func ParseBudget(value string) (time.Duration, error) {
	if value == "" {
		return workspace.DefaultBudget, nil
	}
	budget, err := time.ParseDuration(value)
	if err != nil || budget <= 0 {
		return 0, errors.New("workspace budget must be a positive finite duration")
	}
	return budget, nil
}

// managedRunnerRequestError is the one refusal for a managed workspace request
// that does not use the managed runner. It matches task.ErrInvalidPermit for
// existing consumers, carries no request value, and has no cause so a wrapped
// copy stays recognizable as this exact refusal and nothing broader.
type managedRunnerRequestError struct{}

var errManagedRunnerRequest error = managedRunnerRequestError{}

func (managedRunnerRequestError) Error() string {
	return "managed workspaces require the managed delegate-run runner"
}

func (managedRunnerRequestError) Is(target error) bool { return target == task.ErrInvalidPermit }

// IsManagedRunnerRequest reports whether leaf is the exact refusal that
// RequireManagedRunner returns. It is a leaf predicate: task.AllErrorCauses
// unwraps ordinary wrapping before calling it.
func IsManagedRunnerRequest(leaf error) bool {
	return leaf == errManagedRunnerRequest //nolint:errorlint // exact leaf identity; the caller's traversal already unwrapped
}

// RequireManagedRunner refuses a request that selects a managed workspace and
// asks for a custom runner. A request with no managed workspace selector is
// not judged here.
func RequireManagedRunner(a DispatchRequest) error {
	if a.Isolation == "" && a.WorkspaceID == "" {
		return nil
	}
	if a.RunnerOwnership != task.RunnerOwnershipManaged {
		return errManagedRunnerRequest
	}
	return nil
}
