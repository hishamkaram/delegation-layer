package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func RequestedRunnerOwnership(selection AdmissionSelection) string {
	if selection.RunnerOwnership != "" {
		return selection.RunnerOwnership
	}
	if selection.Runner != "" {
		return task.RunnerOwnershipCustom
	}
	return task.RunnerOwnershipManaged
}
