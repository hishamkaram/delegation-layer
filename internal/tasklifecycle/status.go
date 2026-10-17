package tasklifecycle

import (
	"context"
	"errors"
	"os"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Status(store *taskdir.Store, td *taskdir.TaskDir, options SupervisorOptions, runner string, catalog commonprovider.Catalog) (result StatusResult, resultErr error) {
	inspection, err := td.Inspect()
	interaction, interactionErr := ReadInteraction(td)
	result.Interaction, result.Inspection = interaction, inspection
	if !supervisorcore.TaskTerminal(inspection) {
		err = errors.Join(err, interactionErr)
	}
	if err != nil {
		return result, err
	}
	if supervisorcore.TaskTerminal(inspection) {
		err = observeTerminalStatus(store.Root, td, options, catalog, &result.Observation)
		return result, err
	}
	req, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return result, err
	}
	submission, err := td.ReadSubmission()
	if errors.Is(err, os.ErrNotExist) {
		result.Admission, result.AdmissionObserved = task.AdmissionNotAdmitted, true
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.ControlObservation = true
	prepared, err := supervisorcore.ObserveTaskSupervisor(context.Background(), store, submission.Supervisor, meta, options.Options, options.InitialExecutable, runner, options.OpenStore)
	if err != nil {
		return result, err
	}
	client := prepared.Client
	if client != nil {
		defer func() { supervisorcore.CloseMaintenance(client, resultErr) }()
	}
	var observation pueue.Observation
	var reconcileErr error
	if client != nil {
		observation, reconcileErr = reconcileStatusAttempt(context.Background(), store.Root, td, req, meta, spec, metaHash, prepared, &result.Observation)
	} else {
		observation, reconcileErr = ReconcileStatus(context.Background(), store.Root, td, req, meta, spec, metaHash, options, &result.Observation)
	}
	reconcileErr = errors.Join(reconcileErr, UpdateStatusStops(context.Background(), td, &result.Observation, observation))
	if records, recordsErr := ReadTimeoutRecords(td); recordsErr == nil {
		offer, offerErr := ObserveTimeoutContinuationWithRecords(store.Root, td, req, meta, records, catalog)
		if offer != nil {
			result.Continuation = offer
		}
		reconcileErr = errors.Join(reconcileErr, offerErr)
	} else {
		reconcileErr = errors.Join(reconcileErr, recordsErr)
	}
	return result, reconcileErr
}

func observeTerminalStatus(root string, td *taskdir.TaskDir, options SupervisorOptions, catalog commonprovider.Catalog, observed *Observation) error {
	req, meta, _, _, err := td.PreparedIdentity()
	if err != nil {
		return err
	}
	supervisorcore.CleanupCompletedTaskFromSaved(root, td, meta, options.Options, options.InitialExecutable)
	if records, recordsErr := ReadTimeoutRecords(td); recordsErr == nil {
		// The outcome stays authoritative when optional timeout metadata
		// is malformed or stale.
		offer, offerErr := ObserveTimeoutContinuationWithRecords(root, td, req, meta, records, catalog)
		if offerErr == nil {
			observed.Continuation = offer
		}
	}
	return nil
}

func ReconcileStatus(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, options SupervisorOptions, observed *Observation) (observation pueue.Observation, resultErr error) {
	if ctx == nil {
		return pueue.Observation{}, context.Canceled
	}
	observed.MaintenanceDeferred = false
	observed.MaintenanceError = nil
	clientOptions := supervisorcore.OptionsWithEnvironment(options.Options, meta.Environment)
	submit, err := td.ReadSubmission()
	if err != nil {
		return pueue.Observation{}, err
	}
	client, err := supervisorcore.NewClient(ctx, root, submit.Supervisor, clientOptions, true, options.InitialExecutable, options.OpenStore)
	if err != nil {
		return pueue.Observation{}, err
	}
	defer func() { supervisorcore.CloseMaintenance(client, resultErr) }()
	return ReconcileStatusWithClient(ctx, root, td, req, meta, spec, metaHash, client, observed)
}
