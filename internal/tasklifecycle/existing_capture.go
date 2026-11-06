package tasklifecycle

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func ValidateExistingDispatchRequest(requestedBudget, configPath string, store *taskdir.Store, requested task.TaskRecord, saved *task.TaskRecord, meta *task.MetaRecord) error {
	return ValidateExistingDispatchRequestContext(context.Background(), requestedBudget, configPath, store, requested, saved, meta)
}

func ValidateExistingDispatchRequestContext(ctx context.Context, requestedBudget, configPath string, store *taskdir.Store, requested task.TaskRecord, saved *task.TaskRecord, meta *task.MetaRecord) error {
	if !SameExistingRequest(requestedBudget, &requested, saved) || (configPath != "" && configPath != meta.SupervisorConfig.ConfigPath) {
		return task.ErrRequestConflict
	}
	if err := workspaces.RecordTaskCaptureContext(ctx, store, *saved); err != nil {
		return admissioncore.WithStage("record-workspace-capture", err)
	}
	return nil
}
