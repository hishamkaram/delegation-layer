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

func ReconcileStatusWithClient(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, client *pueue.Client, response *Observation) (observation pueue.Observation, resultErr error) {
	return reconcileStatusAttempt(ctx, root, td, req, meta, spec, metaHash, supervisorcore.TaskSupervisorObservation{Client: client}, response, nil, nil)
}

// ReconcileStatusWithClientAndAdmissionContext observes first. Only a matched
// Stashed row enters outer root admission before recovery actuates the queue.
func ReconcileStatusWithClientAndAdmissionContext(ctx context.Context, store *taskdir.Store, borrowed *taskdir.RootAdmissionLease, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, client *pueue.Client, response *Observation) (pueue.Observation, error) {
	if store == nil {
		return pueue.Observation{}, task.ErrInvalidPermit
	}
	return reconcileStatusAttempt(ctx, store.Root, td, req, meta, spec, metaHash, supervisorcore.TaskSupervisorObservation{Client: client}, response, store, borrowed)
}

func reconcileStatusAttempt(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, prepared supervisorcore.TaskSupervisorObservation, response *Observation, store *taskdir.Store, borrowed *taskdir.RootAdmissionLease) (observation pueue.Observation, resultErr error) {
	if ctx == nil {
		return pueue.Observation{}, context.Canceled
	}
	client := prepared.Client
	if client == nil {
		return pueue.Observation{}, task.ErrInvalidPermit
	}
	response.MaintenanceDeferred = prepared.MaintenanceDeferred
	response.MaintenanceError = prepared.MaintenanceError
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return pueue.Observation{}, receiptErr
	}
	statusClient := client
	if pueue.IsPrivateConfig(root, client.Binding().ConfigPath) {
		statusClient, receiptErr = client.WithObservationTimeout(pueue.PrivateStatusObservationTimeout)
		if receiptErr != nil {
			return pueue.Observation{}, receiptErr
		}
	}
	observation, reconcileErr := statusClient.ReconcileStatus(ctx, TaskIdentity(req, meta, spec, metaHash, receipt))
	if observation.Matched && observation.State == pueue.StateStashed && !prepared.MaintenanceDeferred {
		var recoverErr error
		observation, recoverErr = observeStashedRecovery(ctx, root, td, statusClient, observation, response, store, borrowed)
		reconcileErr = errors.Join(reconcileErr, recoverErr)
	}
	response.Supervisor = DescribeSupervisor(observation)
	response.AdmissionObserved = true
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
	return observation, reconcileErr
}

func observeStashedRecovery(ctx context.Context, root string, td *taskdir.TaskDir, client *pueue.Client, observation pueue.Observation, response *Observation, store *taskdir.Store, borrowed *taskdir.RootAdmissionLease) (pueue.Observation, error) {
	var recovered pueue.Observation
	var err error
	if store == nil {
		recovered, err = RecoverStagedSubmission(ctx, root, td, client, observation)
	} else {
		recovered, err = RecoverStagedSubmissionWithAdmissionContext(ctx, store, borrowed, td, client, observation)
	}
	if admissionCleanupFault(err) || !supervisorcore.TaskMaintenanceCompletionRefused(err) {
		return recovered, err
	}
	// No enqueue was attempted. The actual read lease revalidates canonical
	// observability, including for an overlapping cached snapshot. This is a
	// fresh observation of the saved task, never a retry of its mutation.
	current, observeErr := client.ReconcileStatus(ctx, observation.Identity)
	if !current.Matched {
		observeErr = errors.Join(observeErr, pueue.ErrUnknown)
	}
	if observeErr != nil {
		return current, errors.Join(err, observeErr)
	}
	response.MaintenanceDeferred = true
	response.MaintenanceError = errors.Join(response.MaintenanceError, err)
	return current, nil
}
