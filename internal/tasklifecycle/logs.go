package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ReadLogs(td *taskdir.TaskDir) (*taskdir.TaskInspection, []taskdir.LogDescriptor, error) {
	return ReadLogsContext(context.Background(), td)
}

func ReadLogsContext(ctx context.Context, td *taskdir.TaskDir) (*taskdir.TaskInspection, []taskdir.LogDescriptor, error) {
	inspection, err := td.InspectContext(ctx)
	if err != nil {
		return inspection, nil, err
	}
	descriptors, err := td.LogDescriptorsContext(ctx)
	return inspection, descriptors, err
}
