package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// RejectedRecoveryAvailable is advisory. A failed or unavailable assessment
// never changes the original publication or starts provider work.
func RejectedRecoveryAvailable(td *taskdir.TaskDir, registry predicate.Registry, publication string, outcome *task.OutcomeRecord, resultErr error) *bool {
	if publication != task.PublicationRejected.String() || outcome == nil || resultErr != nil {
		return nil
	}
	offer := false
	interpreter, err := registry.ResolveRecovery(outcome.Predicate)
	if err == nil && td != nil {
		available, availabilityErr := td.RecheckAvailable(interpreter)
		if availabilityErr == nil && available {
			offer = true
		}
	}
	return &offer
}
