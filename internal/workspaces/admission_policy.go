package workspaces

import (
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func DispatchDigest(a DispatchRequest, id, cwd string, brief []byte, budget time.Duration, resource *workspace.Resource) (string, error) {
	identity := dispatchIdentity{TaskID: id, Provider: a.Provider, Auto: a.Auto, SourceCwd: cwd, WorkspaceID: a.WorkspaceID, Base: a.Base, Config: a.Config, BriefSHA256: task.ComputeSHA256(brief), WorkspaceBudgetNanos: int64(budget)}
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
