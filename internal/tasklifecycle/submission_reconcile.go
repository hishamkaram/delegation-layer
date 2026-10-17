package tasklifecycle

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ReconcileSubmitted(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, submit *task.SubmitRecord, options SupervisorOptions) (result SubmissionResult) {
	supervisorOptions := supervisorcore.OptionsWithEnvironment(options.Options, meta.Environment)
	client, err := supervisorcore.NewClient(context.Background(), root, submit.Supervisor, supervisorOptions, true, options.InitialExecutable, options.OpenStore)
	if err != nil {
		return submissionFailure(SubmissionClient, err)
	}
	defer func() { supervisorcore.CloseMaintenance(client, result.Err()) }()
	_, _, spec, metaHash, hashErr := td.PreparedIdentity()
	if hashErr != nil {
		return submissionFailure(SubmissionRecords, hashErr)
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return submissionFailure(SubmissionRecords, receiptErr)
	}
	observation, reconcileErr := client.Reconcile(context.Background(), TaskIdentity(req, meta, spec, metaHash, receipt))
	if observation.Matched && observation.State == pueue.StateStashed {
		var recoverErr error
		observation, recoverErr = RecoverStagedSubmission(context.Background(), root, td, client, observation)
		reconcileErr = errors.Join(reconcileErr, recoverErr)
	}
	response := &result.Observation
	response.Supervisor = DescribeSupervisor(observation)
	response.LivenessObserved, response.AdmissionObserved = true, true
	response.Liveness = LivenessFromQueue(observation.State)
	response.Pending = observedPending(observation)
	if observation.Matched {
		receiptErr := RequireDurableSupervisorReceipt(td, observation)
		reconcileErr = errors.Join(reconcileErr, receiptErr)
		if receiptErr == nil {
			response.Admission = task.AdmissionAdmitted
		} else {
			response.Admission = task.AdmissionUnknown
		}
	} else {
		response.Admission = task.AdmissionUnknown
	}
	if reconcileErr != nil {
		result.Stage, result.Failure = SubmissionObservation, reconcileErr
		return result
	}
	return result
}
