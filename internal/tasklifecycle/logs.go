package tasklifecycle

import "github.com/hishamkaram/delegation-layer/internal/taskdir"

func ReadLogs(td *taskdir.TaskDir) (*taskdir.TaskInspection, []taskdir.LogDescriptor, error) {
	inspection, err := td.Inspect()
	if err != nil {
		return inspection, nil, err
	}
	descriptors, err := td.LogDescriptors()
	return inspection, descriptors, err
}
