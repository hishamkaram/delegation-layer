package supervisor

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveInspectionMaintenanceQueue consumes a complete caller-owned snapshot
// under the existing generation exclusion. Numeric IDs never establish identity.
func ObserveInspectionMaintenanceQueue(ctx context.Context, store *taskdir.Store, saved, current task.SupervisorRef, label string, jobs []pueue.Job) (inspection.MaintenanceQueue, error) {
	if store == nil || label == "" {
		return inspection.MaintenanceQueue{}, task.ErrInvalidPermit
	}
	matches, err := pueue.PrivateSupervisorMaintenanceGenerationMatchesWithLock(ctx, store.Root, saved, current)
	if err != nil {
		return inspection.MaintenanceQueue{}, err
	}
	if !matches {
		return inspection.MaintenanceQueue{}, task.ErrIdentityMismatch
	}
	queue := inspection.MaintenanceQueue{Label: label, State: inspection.MaintenanceQueueAbsent}
	seen := make(map[int64]bool, len(jobs))
	for _, job := range jobs {
		if job.ID < 0 || seen[job.ID] {
			return inspection.MaintenanceQueue{}, task.ErrEvidenceFault
		}
		seen[job.ID] = true
		if job.Label == nil || *job.Label != label {
			continue
		}
		if queue.State == inspection.MaintenanceQueueEnded {
			return inspection.MaintenanceQueue{}, task.ErrIdentityMismatch
		}
		if job.State == pueue.StateUnknown {
			return inspection.MaintenanceQueue{}, fmt.Errorf("%w: exact inspection queue state is unknown", pueue.ErrUnknown)
		}
		if job.State != pueue.StateEnded {
			return inspection.MaintenanceQueue{}, fmt.Errorf("%w: inspection maintenance requires an ended or absent exact queue label", pueue.ErrPrivateSupervisorMigrationBusy)
		}
		id := job.ID
		queue.State = inspection.MaintenanceQueueEnded
		queue.NumericTaskID = &id
	}
	return queue, ctx.Err()
}
