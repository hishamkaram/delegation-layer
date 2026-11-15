package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveAdministrativeSettlement stops observation without granting result
// readiness. Existing publication, a collectible seal or canonical native
// closure take precedence, including over a malformed unrelated sidecar.
func ObserveAdministrativeSettlement(ctx context.Context, td *taskdir.TaskDir, inspection *taskdir.TaskInspection) (*task.OperatorDispositionRecord, error) {
	if inspection != nil && (inspection.Terminal() || inspection.SealExists) {
		return nil, nil
	}
	record, administrativeErr := td.ReadOperatorDispositionContext(ctx)
	if task.RecordAbsent(administrativeErr) {
		return nil, nil
	}
	evidence, err := taskdir.LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return nil, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || closure.Closed() {
		return nil, err
	}
	return record, administrativeErr
}
