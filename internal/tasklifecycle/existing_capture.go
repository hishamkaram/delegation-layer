package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func ValidateExistingDispatchRequest(requestedBudget, configPath string, store *taskdir.Store, requested task.TaskRecord, saved *task.TaskRecord, meta *task.MetaRecord) error {
	if !SameExistingRequest(requestedBudget, &requested, saved) || (configPath != "" && configPath != meta.SupervisorConfig.ConfigPath) {
		return task.ErrRequestConflict
	}
	if err := workspaces.RecordTaskCapture(store, *saved); err != nil {
		return admissioncore.WithStage("record-workspace-capture", err)
	}
	return nil
}
