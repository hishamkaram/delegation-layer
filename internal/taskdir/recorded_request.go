package taskdir

import (
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RecordedRequest reads a validated immutable request even when initialization
// stopped before meta.json. It grants no launch authority: submission/recovery
// must still validate the complete prepared set and its guards.
func (td *TaskDir) RecordedRequest() (*task.TaskRecord, error) {
	data, err := td.store.readBytes(filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	request, err := td.decodeTaskRequest(data)
	if err != nil {
		return nil, err
	}
	return &request, nil
}

func (td *TaskDir) decodeTaskRequest(data []byte) (task.TaskRecord, error) {
	var request task.TaskRecord
	if err := task.DecodeStrict(data, &request); err != nil {
		return task.TaskRecord{}, err
	}
	if err := task.ValidateTaskRecord(&request); err != nil {
		return task.TaskRecord{}, err
	}
	if request.RootID != td.store.RootID || request.TaskID != td.TaskID {
		return task.TaskRecord{}, task.ErrIdentityMismatch
	}
	return request, nil
}
