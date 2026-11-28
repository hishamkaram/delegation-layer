package tasklifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/config/policy"
	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ResolveRoot(raw string) (string, error) {
	if raw == "" {
		var err error
		raw, err = config.ResolveDefaultRoot()
		if err != nil {
			return "", err
		}
	}
	return config.CanonicalizePath(raw)
}

func dispatchTaskID(a DispatchRequest) (string, error) {
	if a.TaskID != "" {
		return a.TaskID, nil
	}
	if a.GeneratedTaskID != "" {
		return a.GeneratedTaskID, nil
	}
	return randomid.NewTaskID()
}

func dispatchInputs(a DispatchRequest) (root, cwd string, brief []byte, err error) {
	if _, err = policy.ResolveExecutionBudget("", a.Config.Budget, a.BudgetIntent); err != nil {
		return "", "", nil, err
	}
	return dispatchContentInputs(a)
}

// Existing-only reconciliation reads content before recovering the saved
// request's budget. Fresh dispatch must use dispatchInputs instead.
func dispatchContentInputs(a DispatchRequest) (root, cwd string, brief []byte, err error) {
	root, err = ResolveRoot(a.Root)
	if err != nil {
		return "", "", nil, err
	}
	if a.WorkspaceID == "" {
		_, cwd, err = config.ValidateDirectories(root, a.Cwd)
		if err != nil {
			return "", "", nil, err
		}
	} else {
		cwd = a.Cwd
	}
	briefPath := a.BriefPath
	if a.BriefBytes == nil {
		var pathErr error
		briefPath, pathErr = filepath.Abs(briefPath)
		if pathErr != nil {
			return "", "", nil, pathErr
		}
	}
	brief, err = ReadBriefInput(briefPath, a.BriefBytes)
	return root, cwd, brief, err
}

func persistedProviderForTask(store *taskdir.Store, taskID string) (provider string, exists bool, resultErr error) {
	td, err := store.OpenTask(taskID)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	req, _, err := td.PreparedRecords()
	if err != nil {
		return "", true, fmt.Errorf("existing task cannot be reused for automatic provider selection: %w", err)
	}
	if req == nil || req.Provider == "" {
		return "", true, errors.New("existing task has no persisted provider")
	}
	return req.Provider, true, nil
}

func buildDispatchTask(rootID, id, cwd string, brief []byte, request DispatchRequest) (task.TaskRecord, error) {
	budget, err := policy.ResolveExecutionBudget(request.Provider, request.Config.Budget, request.BudgetIntent)
	if err != nil {
		return task.TaskRecord{}, err
	}
	resolved := request.Config
	resolved.Budget = budget.String()
	return admissioncore.BuildRequest(rootID, id, request.Provider, cwd, resolved, brief, nil)
}
