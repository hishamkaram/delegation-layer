package tasklifecycle

import (
	"context"
	"errors"
	"os"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Status(store *taskdir.Store, td *taskdir.TaskDir, options SupervisorOptions, runner string, catalog commonprovider.Catalog) (result StatusResult, resultErr error) {
	return StatusContext(context.Background(), store, td, options, runner, catalog)
}

func StatusContext(ctx context.Context, store *taskdir.Store, td *taskdir.TaskDir, options SupervisorOptions, runner string, catalog commonprovider.Catalog) (result StatusResult, resultErr error) {
	options.AdmissionStore = store
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
		err = observeTerminalStatus(ctx, store.Root, td, options, catalog, &result.Observation)
		return result, err
	}
	req, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return result, err
	}
	submission, err := td.ReadSubmission()
	if errors.Is(err, os.ErrNotExist) {
		result.Admission, result.AdmissionObserved = task.AdmissionNotAdmitted, true
		// An unsubmitted task can carry a permanent automatic refusal. Expose
		// its truthful cause without inventing execution or terminal outcome.
		result.Stops, err = ReadTimeoutRecords(td)
		result.StopsRead = err == nil
		return result, err
	}
	if err != nil {
		return result, err
	}
	result.ControlObservation = true
	prepared, err := options.observeSubmittedSupervisor(ctx, store, submission, meta, runner)
	if err != nil && !IsPureSupervisorUncertaintyError(err) {
		// A successful preparation may return its client beside a later owned
		// RootSH release fault. Dispose the client before returning that owner error.
		return result, supervisorcore.ClosePrivateAdmissionTransition(prepared.Client, err)
	}
	var observation pueue.Observation
	var reconcileErr error
	switch client := prepared.Client; {
	case err != nil:
		// Pure preparation uncertainty completed no queue observation, so it
		// reports only its unfinished command. The local durable records still
		// own any interruption or timeout offer derived below.
		reconcileErr = err
		result.Pending = uncertaintyPending(err)
	case client != nil:
		defer func() { supervisorcore.CloseMaintenance(client, resultErr) }()
		observation, reconcileErr = reconcileStatusAttempt(ctx, store.Root, td, req, meta, spec, metaHash, prepared, &result.Observation)
	default:
		observation, reconcileErr = ReconcileStatus(ctx, store.Root, td, req, meta, spec, metaHash, options, &result.Observation)
	}
	reconcileErr = errors.Join(reconcileErr, UpdateStatusStops(ctx, td, &result.Observation, observation))
	if reconcileErr != nil && !IsPureSupervisorUncertaintyError(reconcileErr) {
		// A concrete fault is the authoritative result; the optional
		// continuation offer must not advertise guidance beside it.
		return result, reconcileErr
	}
	if records, recordsErr := ReadTimeoutRecords(td); recordsErr == nil {
		offer, offerErr := ObserveTimeoutContinuationWithRecordsContext(ctx, store.Root, td, req, meta, records, catalog)
		if offer != nil {
			result.Continuation = offer
		}
		reconcileErr = errors.Join(reconcileErr, offerErr)
	} else {
		reconcileErr = errors.Join(reconcileErr, recordsErr)
	}
	return result, reconcileErr
}

func observeTerminalStatus(ctx context.Context, root string, td *taskdir.TaskDir, options SupervisorOptions, catalog commonprovider.Catalog, observed *Observation) error {
	req, meta, _, _, err := td.PreparedIdentity()
	if err != nil {
		return err
	}
	if records, recordsErr := ReadTimeoutRecords(td); recordsErr == nil {
		// The outcome stays authoritative when optional timeout metadata
		// is malformed or stale.
		offer, offerErr := ObserveTimeoutContinuationWithRecordsContext(ctx, root, td, req, meta, records, catalog)
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
	source, err := supervisorcore.NewSavedBindingSource(req.RootID, infrastructure.SavedBindingTaskSubmission, td.TaskID, "", submit.Supervisor)
	if err != nil {
		return pueue.Observation{}, err
	}
	options.Options = clientOptions
	client, err := options.savedClient(ctx, root, submit.Supervisor, source)
	if err != nil {
		return pueue.Observation{}, supervisorcore.ClosePrivateAdmissionTransition(client, err)
	}
	defer func() { supervisorcore.CloseMaintenance(client, resultErr) }()
	return ReconcileStatusWithClient(ctx, root, td, req, meta, spec, metaHash, client, observed)
}
