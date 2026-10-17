package testsupport

import (
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ContinuationSupervisorRef(root, digest, endpoint string) task.SupervisorRef {
	return supervisorfixture.ContinuationSupervisorRef(root, digest, endpoint)
}
