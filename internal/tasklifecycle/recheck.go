package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Recheck(td *taskdir.TaskDir, registry predicate.Registry) (result Reassessment, resultErr error) {
	inspection, err := td.Inspect()
	result.Inspection = inspection
	if err != nil {
		return result, err
	}
	result.Stage = RecheckEligibility
	if inspection == nil || inspection.Outcome == nil || inspection.Publication != task.PublicationRejected || inspection.Outcome.Verdict != task.VerdictRejected {
		return result, task.ErrRecheckNotRejected
	}
	result.Stage = RecheckInterpreter
	interpreter, err := registry.ResolveRecovery(inspection.Outcome.Predicate)
	if err != nil {
		return result, err
	}
	result.Stage = RecheckAssessment
	result.Assessment, err = td.Recheck(interpreter)
	return result, err
}
