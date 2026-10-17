package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// privateQueueAuthorizationWorkers bounds only the filesystem work used to
// publish queue observations. It does not limit admitted or running tasks.
const privateQueueAuthorizationWorkers = 32

type privateCleanupCandidate struct {
	job      pueue.Job
	taskID   string
	settling bool
}

type privateCleanupDecision struct {
	remove   bool
	settling bool
}
