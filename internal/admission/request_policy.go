package admission

import (
	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func BuildRequest(rootID, id, provider, cwd string, requested task.TaskConfig, brief []byte, prior *task.PriorSession) (task.TaskRecord, error) {
	budget, err := config.ParseBudgetForProvider(provider, requested.Budget)
	if err != nil {
		return task.TaskRecord{}, err
	}
	req := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, TaskID: id, Provider: provider, Mode: requested.Permission, CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(budget), PriorSession: prior, BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	if err = task.NormalizeRequestedConfig(&req); err != nil {
		return task.TaskRecord{}, err
	}
	return req, nil
}
