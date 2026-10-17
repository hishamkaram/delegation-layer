package tasklifecycle

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func SubmitWithClient(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, runner string, permit *taskdir.SubmissionPermit) SubmissionResult {
	result := SubmissionResult{Stage: SubmissionObservation}
	response := &result.Observation
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	ownership, err := supervisorcore.RunnerOwnershipForTask(root, td, meta)
	if err != nil {
		result.Failure = err
		return result
	}
	commit := func(observation pueue.Observation) error {
		receipt, receiptErr := observation.Receipt()
		if receiptErr != nil {
			return receiptErr
		}
		return td.RecordSupervisorReceipt(*receipt)
	}
	observation, err := client.SubmitStagedWithGate(context.Background(), permit, pueue.Launch{
		RunnerExecutable: runner,
		RootPath:         root,
		Supervised:       ownership == task.RunnerOwnershipManaged,
	}, supervisorcore.SubmissionGate(root), commit)
	response.Supervisor = DescribeSupervisor(observation)
	response.LivenessObserved, response.AdmissionObserved = true, true
	response.Liveness = LivenessFromQueue(observation.State)
	response.Pending = observedPending(observation)
	if observation.Matched {
		// A matched Stashed row proves supervisor acceptance. The staged commit
		// publishes its exact receipt before enqueue, so a commit or enqueue
		// failure leaves recoverable work that status can safely resume.
		response.Admission = task.AdmissionAdmitted
	} else {
		response.Admission = task.AdmissionUnknown
	}
	if req != nil && meta != nil {
		inspection, inspectErr := td.Inspect()
		response.Inspection = inspection
		err = errors.Join(err, inspectErr)
	}
	if err != nil {
		result.Failure = err
		return result
	}
	return result
}
