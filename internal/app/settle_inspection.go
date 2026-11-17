package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func settleInspectionContext(ctx context.Context, a Arguments, store *taskdir.Store, response Response, deps Dependencies) (result commandResult) {
	operation, err := inspection.LoadOperationContext(ctx, store, a.TaskID)
	if err != nil {
		var fact *task.ControlFactError
		if !errors.As(err, &fact) {
			err = &task.ControlFactError{ResourceKind: "inspection", ResourceID: a.TaskID, FactKind: "inspection_record", Name: "inspections/" + a.TaskID, ReasonCode: "inspection_evidence_unavailable", NextAction: "inspect_inspection", Cause: err}
		}
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, operation.Close) }()
	err = withSettlementBootstrap(ctx, store, func(locked context.Context) error {
		var actionErr error
		response.Settlement, actionErr = settleInspectionLocked(locked, a, store, operation, deps)
		return actionErr
	})
	if err != nil {
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func settleInspectionLocked(ctx context.Context, a Arguments, store *taskdir.Store, operation *inspection.Operation, deps Dependencies) (*SettlementResponse, error) {
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	if a.SettlementConfirm != "" {
		existing, readErr := confirmedInspectionSettlement(ctx, operation, pin, a.SettlementConfirm)
		if readErr != nil || existing != nil {
			return existing, readErr
		}
	}
	if err = inspectionSettlementEligibility(ctx, store, a.TaskID); err != nil {
		return nil, err
	}
	queue, err := observeInspectionSettlementQueue(ctx, store, operation, deps)
	if err != nil {
		return nil, err
	}
	if a.SettlementPrepare {
		_, challenge, prepareErr := operation.PrepareMaintenanceDispositionContext(ctx, pin, queue, lifetime.NativeObservation{})
		if prepareErr != nil {
			return nil, prepareErr
		}
		return inspectionSettlementResponse(challenge, a.TaskID), nil
	}
	record, err := operation.ConfirmMaintenanceDispositionContext(ctx, pin, queue, a.SettlementConfirm, lifetime.NativeObservation{})
	if record == nil {
		return nil, err
	}
	response := inspectionSettlementResponse(record.Challenge, a.TaskID)
	response.AdministrativeState = "settled"
	response.ConfirmedAt = record.ConfirmedAt
	return response, err
}

func confirmedInspectionSettlement(ctx context.Context, operation *inspection.Operation, pin taskdir.RootPin, challenge string) (*SettlementResponse, error) {
	existing, err := operation.ReadMaintenanceDispositionContext(ctx, pin)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.Challenge != challenge {
		return nil, task.ErrIdentityMismatch
	}
	response := inspectionSettlementResponse(existing.Challenge, existing.Subject.OperationID)
	response.AdministrativeState = "settled"
	response.ConfirmedAt = existing.ConfirmedAt
	return response, nil
}

func observeInspectionSettlementQueue(ctx context.Context, store *taskdir.Store, operation *inspection.Operation, deps Dependencies) (queue inspection.MaintenanceQueue, resultErr error) {
	request := operation.Request()
	options := supervisor.OptionsWithEnvironment(supervisorControlOptions(deps), request.Binding.Environment)
	client, err := pueue.ProbeSavedPrivateMaintenanceWithLock(ctx, store.Root, request.Binding.Supervisor, options, func(context.Context) (string, string, error) { return migrationSupervisorPair(deps.supervisorLookup()) })
	if err != nil {
		return queue, err
	}
	if client == nil {
		return queue, &task.ControlFactError{ResourceKind: "inspection", ResourceID: request.IdentityID(), FactKind: "queue_observation", Name: request.Label, ReasonCode: "supervisor_unobservable", NextAction: "inspect_supervisor", Cause: pueue.ErrUnknown}
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return queue, err
	}
	return supervisor.ObserveInspectionMaintenanceQueue(ctx, store, request.Binding.Supervisor, client.Binding(), request.Label, snapshot.Jobs)
}

// Journal-owned eligibility is checked before transport: automatic recovery
// and terminal evidence never depend on removed executables or reused queue IDs.
func inspectionSettlementEligibility(ctx context.Context, store *taskdir.Store, id string) error {
	return inspection.ObserveMaintenanceContext(ctx, store, id, inspection.MaintenanceQueue{}, lifetime.NativeObservation{}, func(observed inspection.MaintenanceObservation) error {
		_, proofErr := guardian.ReadAuxiliaryClosureProof(ctx, observed.Lifetime.Records)
		if proofErr != nil && !errors.Is(proofErr, guardian.ErrProofPending) {
			return proofErr
		}
		if observed.HistoricalExit || proofErr == nil {
			return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "maintenance_eligibility", Name: "terminal evidence", ReasonCode: "maintenance_already_eligible", NextAction: "run_supervisor_ensure_owner", Cause: task.ErrTerminalTask}
		}
		if observed.RecoveryPending {
			return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "lifetime_recovery", Name: "tracked inspection", ReasonCode: "automatic_recovery_available", NextAction: "run_supervisor_ensure_owner", Cause: guardian.ErrProofPending}
		}
		if observed.Lifetime.PreparationOnly {
			reason, action := "awaiting_deadline", "wait_for_deadline"
			if !time.Now().Before(observed.Deadline) {
				reason, action = "automatic_recovery_available", "run_supervisor_ensure_owner"
			}
			return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "lifetime_recovery", Name: "unstarted inspection", ReasonCode: reason, NextAction: action, Cause: guardian.ErrProofPending}
		}
		return nil
	})
}

func inspectionSettlementResponse(challenge, id string) *SettlementResponse {
	response := settlementResponse(challenge, nil)
	response.TargetKind = "inspection"
	response.OperationID = id
	response.AcknowledgedMissingFacts = []string{"scope_closure_unproven"}
	return response
}
