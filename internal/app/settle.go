package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

var _ taskdir.OperatorDispositionNative = lifetime.NativeObservation{}

// SettlementResponse keeps human maintenance permission separate from native
// execution and publication. A challenge proves freshness, never human identity.
type SettlementResponse struct {
	AdministrativeState string   `json:"administrative_state"`
	NativeExecution     string   `json:"native_execution"`
	Permission          string   `json:"permission"`
	Challenge           string   `json:"challenge"`
	ConfirmedAt         string   `json:"confirmed_at,omitempty"`
	HeldClaims          []string `json:"held_claims"`
}

func settlementResponse(challenge string, record *task.OperatorDispositionRecord) *SettlementResponse {
	response := &SettlementResponse{
		AdministrativeState: "prepared", NativeExecution: "unknown", Permission: task.OperatorPermissionSupervisorMaintenance, Challenge: challenge,
		HeldClaims: []string{"scheduler", "resource", "session", "workspace", "custody", "deletion"},
	}
	if record != nil {
		response.AdministrativeState, response.ConfirmedAt = "settled", record.ConfirmedAt
	}
	return response
}

func settle(a Arguments, deps Dependencies) commandResult {
	return settleContext(context.Background(), a, deps)
}

func settleContext(parent context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("settle")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	options := supervisorControlOptions(deps)
	ctx, cancel := supervisor.ControlContextWithParent(parent, options)
	defer cancel()
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { closeObservationStore(&result, store) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	err = pueue.WithExistingBootstrapLockContext(ctx, root, func(locked context.Context) error {
		var actionErr error
		response.Settlement, actionErr = settleTaskContext(locked, a, store, td, deps)
		return actionErr
	})
	if err != nil {
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func observeSettlementQueue(ctx context.Context, store *taskdir.Store, td *taskdir.TaskDir, options pueue.Options, deps Dependencies) (queue task.OperatorDispositionQueue, resultErr error) {
	req, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return queue, err
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return queue, err
	}
	if !pueue.IsPrivateConfig(store.Root, receipt.ConfigPath) {
		return queue, fmt.Errorf("%w: settlement requires the task's private supervisor", task.ErrIdentityMismatch)
	}
	options = supervisor.OptionsWithEnvironment(options, meta.Environment)
	client, err := pueue.ProbeSavedPrivateMaintenanceWithLock(ctx, store.Root, receipt.SupervisorRef(), options, func(context.Context) (string, string, error) {
		return migrationSupervisorPair(deps.supervisorLookup())
	})
	if err != nil {
		return queue, err
	}
	if client == nil {
		return queue, fmt.Errorf("%w: the exact private supervisor is not observable", pueue.ErrUnknown)
	}
	defer func() { resultErr = errors.Join(resultErr, client.Close()) }()
	observed, err := client.Reconcile(ctx, tasklifecycle.TaskIdentity(req, meta, spec, metaHash, receipt))
	if err != nil {
		return queue, err
	}
	if !observed.Matched || observed.State != pueue.StateEnded || observed.NumericTaskID != receipt.NumericTaskID {
		return queue, fmt.Errorf("%w: settlement requires the exact ended queue row", task.ErrIdentityMismatch)
	}
	return task.OperatorDispositionQueue{NumericTaskID: observed.NumericTaskID, Label: receipt.Label, Ended: true}, nil
}

func supervisorMaintenanceCommand(a Arguments, deps Dependencies) commandResult {
	if a.Command == "settle" {
		return settle(a, deps)
	}
	return supervisorCommand(a, deps)
}

func validateSettlementArguments(a *Arguments, positional []string) error {
	if len(positional) != 1 || a.SettlementPrepare == (a.SettlementConfirm != "") {
		return errors.New("settle requires one local task ID and exactly one of --prepare or --confirm CHALLENGE")
	}
	a.TaskID = positional[0]
	if err := task.ValidateTaskID(a.TaskID); err != nil {
		return err
	}
	if a.SettlementConfirm != "" {
		return task.ValidateSHA256(a.SettlementConfirm)
	}
	return nil
}

func settleTaskContext(ctx context.Context, a Arguments, store *taskdir.Store, td *taskdir.TaskDir, deps Dependencies) (response *SettlementResponse, resultErr error) {
	options := supervisorControlOptions(deps)

	if a.SettlementConfirm != "" {
		_, readErr := td.ReadOperatorDispositionContext(ctx)
		if readErr == nil {
			winner, confirmErr := td.ConfirmOperatorDispositionContext(ctx, task.OperatorDispositionQueue{}, a.SettlementConfirm, lifetime.NativeObservation{})
			if winner != nil {
				response = settlementResponse(winner.Challenge, winner)
			}
			return response, confirmErr
		}
		if !task.RecordAbsent(readErr) {
			return response, readErr
		}
	}
	queue, queueErr := observeSettlementQueue(ctx, store, td, options, deps)
	if queueErr != nil {
		return nil, queueErr
	}
	if a.SettlementPrepare {
		_, challenge, prepareErr := td.PrepareOperatorDispositionContext(ctx, queue, lifetime.NativeObservation{})
		if prepareErr == nil {
			response = settlementResponse(challenge, nil)
		}
		return response, prepareErr
	}
	winner, confirmErr := td.ConfirmOperatorDispositionContext(ctx, queue, a.SettlementConfirm, lifetime.NativeObservation{})
	if winner != nil {
		response = settlementResponse(winner.Challenge, winner)
	}
	return response, confirmErr
}

func validateTaskReviewArguments(a *Arguments, positional []string) error {
	if a.Command == "settle" {
		return validateSettlementArguments(a, positional)
	}
	return a.validateRecheck(positional)
}
