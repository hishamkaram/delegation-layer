package app

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func validateExistingDispatchRequest(a Arguments, store *taskdir.Store, requested task.TaskRecord, saved *task.TaskRecord, meta *task.MetaRecord) error {
	if !sameExistingRequest(a, &requested, saved) || (a.PueueConfig != "" && a.PueueConfig != meta.SupervisorConfig.ConfigPath) {
		return task.ErrRequestConflict
	}
	if err := recordManagedTaskCapture(store, *saved); err != nil {
		return withDispatchStage("record-workspace-capture", err)
	}
	return nil
}
