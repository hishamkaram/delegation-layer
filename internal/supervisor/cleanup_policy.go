package supervisor

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func validateLegacyEndedInspectionJob(taskID string, job pueue.Job) error {
	if job.State != pueue.StateEnded {
		return fmt.Errorf("%w: Delegate inspection %s has not ended", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}

func LegacyJobTaskID(label, rootID string) (taskID string, managed, inspectionWorker bool, resultErr error) {
	ordinaryPrefix := "delegate:" + rootID + ":"
	inspectionPrefix := "delegation-inspection-" + rootID + "-"
	switch {
	case strings.HasPrefix(label, ordinaryPrefix):
		taskID = strings.TrimPrefix(label, ordinaryPrefix)
	case strings.HasPrefix(label, inspectionPrefix):
		taskID = strings.TrimPrefix(label, inspectionPrefix)
		inspectionWorker = true
	default:
		return "", false, false, nil
	}
	if err := task.ValidateTaskID(taskID); err != nil {
		return "", true, inspectionWorker, err
	}
	return taskID, true, inspectionWorker, nil
}

func TaskTerminal(insp *taskdir.TaskInspection) bool {
	return insp != nil && (insp.Publication == task.PublicationCommitted || insp.Publication == task.PublicationRejected)
}

func privateSuccessfulCleanupCandidates(rootID string, jobs []pueue.Job) []privateCleanupCandidate {
	labelCounts := PrivateQueueLabelCounts(jobs)
	candidates := make([]privateCleanupCandidate, 0, len(jobs))
	for _, job := range jobs {
		if job.Label == nil || labelCounts[*job.Label] != 1 {
			continue
		}
		taskID, managed, inspectionWorker, labelErr := LegacyJobTaskID(*job.Label, rootID)
		if labelErr != nil || !managed {
			continue
		}
		switch job.State {
		case pueue.StateEnded:
			candidates = append(candidates, privateCleanupCandidate{job: job, taskID: taskID})
		case pueue.StateRunning:
			if inspectionWorker {
				candidates = append(candidates, privateCleanupCandidate{job: job, taskID: taskID, settling: true})
			}
		case pueue.StateUnknown, pueue.StateQueued, pueue.StateStashed:
			continue
		}
	}
	return candidates
}

func PrivateQueueLabelCounts(jobs []pueue.Job) map[string]int {
	counts := make(map[string]int)
	for _, job := range jobs {
		if job.Label != nil {
			counts[*job.Label]++
		}
	}
	return counts
}
