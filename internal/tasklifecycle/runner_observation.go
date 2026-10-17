package tasklifecycle

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ReconcileRunner(ctx context.Context, root string, response *Observation, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options SupervisorOptions) (client *pueue.Client, observation pueue.Observation, resultErr error) {
	submit, err := td.ReadSubmission()
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	client, err = supervisorcore.NewClient(ctx, root, submit.Supervisor, options.Options, true, options.InitialExecutable, options.OpenStore)
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	defer func(owner *pueue.Client) {
		if resultErr != nil {
			supervisorcore.CloseMaintenance(owner, resultErr)
			client = nil
		}
	}(client)
	spec, metaHash, err := preparedHashes(td)
	if err != nil {
		return nil, pueue.Observation{}, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil && !errors.Is(receiptErr, os.ErrNotExist) {
		return nil, pueue.Observation{}, receiptErr
	}
	// This runner owns startup until its finite caller deadline; a short
	// watcher timeout must not abandon the admitted task. Keep the original
	// client for stop control and migration cleanup.
	observer := client
	if deadline, bounded := ctx.Deadline(); bounded {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, pueue.Observation{}, context.DeadlineExceeded
		}
		observer, err = client.WithObservationTimeout(remaining)
		if err != nil {
			return nil, pueue.Observation{}, err
		}
	}
	observation, reconcileErr := observer.Reconcile(ctx, TaskIdentity(req, meta, spec, metaHash, receipt))
	response.Supervisor = DescribeSupervisor(observation)
	response.LivenessObserved, response.AdmissionObserved = true, true
	response.Liveness = LivenessFromQueue(observation.State)
	response.Pending = observedPending(observation)
	if !observation.Matched {
		response.Admission = task.AdmissionUnknown
		if reconcileErr == nil {
			reconcileErr = pueue.ErrUnknown
		}
		return client, observation, reconcileErr
	}
	receiptErr = RequireDurableSupervisorReceipt(td, observation)
	if receiptErr == nil {
		response.Admission = task.AdmissionAdmitted
	} else {
		response.Admission = task.AdmissionUnknown
	}
	reconcileErr = errors.Join(reconcileErr, receiptErr)
	return client, observation, reconcileErr
}

func preparedHashes(td *taskdir.TaskDir) (string, string, error) {
	_, _, spec, metaHash, err := td.PreparedIdentity()
	return spec, metaHash, err
}
