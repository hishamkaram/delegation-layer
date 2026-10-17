package tasklifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"

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
	if a.BriefBytes != nil {
		if len(a.BriefBytes) == 0 {
			return "", "", nil, errors.New("brief must be nonempty")
		}
		brief = append([]byte(nil), a.BriefBytes...)
		if len(brief) > task.MaxBriefSize {
			return "", "", nil, config.ErrBriefTooLarge
		}
		return root, cwd, brief, nil
	}
	briefPath, pathErr := filepath.Abs(a.BriefPath)
	if pathErr != nil {
		return "", "", nil, pathErr
	}
	brief, err = ReadBrief(briefPath)
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
