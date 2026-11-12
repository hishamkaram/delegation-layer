package app

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// Custody is a dated resident observation; Phase is the restart journal's
// observation. Neither field establishes provider authentication or task outcome.
type SupervisorRuntimeRestartResponse struct {
	OperationPending bool                                 `json:"operation_pending,omitempty"`
	FromStartup      string                               `json:"from_startup"`
	Phase            infrastructure.RuntimeRestartPhase   `json:"phase,omitempty"`
	SuccessorStartup string                               `json:"successor_startup,omitempty"`
	Custody          infrastructure.RuntimeRestartCustody `json:"custody,omitempty"`
	ObservedAt       string                               `json:"observed_at,omitempty"`
}

func supervisorRuntimeRestart(a Arguments, deps Dependencies) (result commandResult) {
	response := newSupervisorCommandResponse("restart")
	from, err := infrastructure.ParseRuntimeRestartReference(a.SupervisorFromStartup)
	if err != nil {
		return failedSupervisorCause(response, "restart startup token is invalid", err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedSupervisorCause(response, "restart state root is unavailable", err, 1)
	}
	options := supervisorControlOptions(deps)
	ctx, cancel := supervisor.ControlContext(options)
	defer cancel()
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
	if err != nil {
		return failedSupervisorCause(response, "restart requires an existing state root", err, 1)
	}
	defer func() { closeObservationStore(&result, store) }()
	response.RootID = store.RootID
	response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{FromStartup: a.SupervisorFromStartup}
	control := supervisor.ControlRuntime{Options: options, Lookup: deps.supervisorLookup(), Routing: deps.infrastructureRouting()}
	_, report, cause := control.RestartOwn(ctx, store, from)
	return finishRuntimeRestart(response, from, report, cause)
}

func finishRuntimeRestart(response Response, from infrastructure.RuntimeRestartReference, report infrastructure.RuntimeRestartReport, cause error) commandResult {
	control := response.SupervisorControl
	if report != (infrastructure.RuntimeRestartReport{}) {
		if err := report.Validate(); err != nil || report.From != from.Startup {
			return failedSupervisorCause(response, "restart observation could not be validated", errors.Join(cause, err, infrastructure.ErrIdentity), 1)
		}
		control.RuntimeRestart.Phase = report.Phase
		if report.Successor != (infrastructure.StartupReference{}) {
			token, err := infrastructure.FormatRuntimeRestartReference(infrastructure.RuntimeRestartReference{RootID: from.RootID, Startup: report.Successor})
			if err != nil {
				return failedSupervisorCause(response, "restart successor could not be validated", errors.Join(cause, err), 1)
			}
			control.RuntimeRestart.SuccessorStartup = token
		}
	}
	if cause != nil {
		return finishRuntimeRestartFailure(response, report, cause)
	}
	control.State, control.NextAction = "pending", "repeat_same_restart"
	if report.Phase == infrastructure.RuntimeRestartReady {
		control.State, control.NextAction = "restarted", "none"
		return commandResult{response: response}
	}
	return commandResult{response: response, code: 3}
}

func finishRuntimeRestartFailure(response Response, report infrastructure.RuntimeRestartReport, cause error) commandResult {
	control := response.SupervisorControl
	public := publicSupervisorError("runtime restart did not complete; retain the original startup token and inspect this operation", cause)
	var retained interface{ RetainsResidentCleanup() }
	if errors.As(cause, &retained) || taskdir.CleanupHasRetainedStore(cause) {
		return failed(response, public, 1)
	}
	if runtimeRestartPendingOnly(report, cause) {
		control.State, control.NextAction = "pending", "repeat_same_restart"
		return commandResult{response: response, code: 3, err: public}
	}
	if task.AllErrorCauses(cause, func(leaf error) bool {
		return exactRuntimeRestartCause(leaf, supervisor.ErrRuntimeQuiescenceBusy) || exactRuntimeRestartCause(leaf, infrastructure.ErrOwnerBusy) || exactRuntimeRestartCause(leaf, task.ErrLockBusy)
	}) {
		control.State, control.NextAction = "blocked", "retry_later"
		return commandResult{response: response, code: 3, err: public}
	}
	return failed(response, public, 1)
}

// The public exit policy requires an accepted nonterminal report and an exact
// pending marker. A bare deadline, opaque failure or hard cause is not pending.
func runtimeRestartPendingOnly(report infrastructure.RuntimeRestartReport, cause error) bool {
	if report == (infrastructure.RuntimeRestartReport{}) || report.Phase == infrastructure.RuntimeRestartReady {
		return false
	}
	marked := false
	safe := task.AllErrorCauses(cause, func(leaf error) bool {
		if exactRuntimeRestartCause(leaf, pueue.ErrPrivateRuntimeRestartPending) || exactRuntimeRestartCause(leaf, infrastructure.ErrOwnerPending) {
			marked = true
			return true
		}
		return exactRuntimeRestartCause(leaf, context.DeadlineExceeded) || exactRuntimeRestartCause(leaf, context.Canceled) || exactRuntimeRestartCause(leaf, pueue.ErrInFlight)
	})
	return safe && marked
}

// Is aliases cannot turn another leaf into an observation or pending marker.
// Compare only checked comparable values, as the host's wire classifier does.
func exactRuntimeRestartCause(cause, expected error) bool {
	left, right := reflect.ValueOf(cause), reflect.ValueOf(expected)
	return left.IsValid() && right.IsValid() && left.Comparable() && right.Comparable() && left.Type() == right.Type() && left.Equal(right)
}

func supervisorRuntimeRestartPreflight(a Arguments, deps Dependencies) (result commandResult) {
	response := newSupervisorCommandResponse("status")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedSupervisorCause(response, "restart state root is unavailable", err, 1)
	}
	ctx, cancel := supervisor.ControlContext(supervisorControlOptions(deps))
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return failedSupervisorCause(response, "restart preflight requires an existing state root", err, 1)
	}
	defer func() { mergeCommandClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	pending, err := pueue.PrivateRuntimeRestartPending(ctx, root)
	if err != nil {
		return failedSupervisorCause(response, "restart journal could not be observed", err, 1)
	}
	observed, cause := infrastructure.ReadRuntimeRestartPreflight(ctx, reader, infrastructure.StartupReference{})
	if observed == (infrastructure.RuntimeRestartPreflight{}) && cause == nil {
		return failedSupervisorCause(response, "restart custody observation is missing", infrastructure.ErrIdentity, 1)
	}
	if observed != (infrastructure.RuntimeRestartPreflight{}) {
		if err = observed.Validate(); err != nil {
			return failedSupervisorCause(response, "restart custody observation could not be validated", errors.Join(cause, err), 1)
		}
		token, formatErr := infrastructure.FormatRuntimeRestartReference(observed.From)
		if formatErr != nil {
			return failedSupervisorCause(response, "restart startup could not be validated", errors.Join(cause, formatErr), 1)
		}
		response.SupervisorControl.RuntimeRestart = &SupervisorRuntimeRestartResponse{FromStartup: token, Custody: observed.Custody, ObservedAt: observed.ObservedAt, OperationPending: pending}
	}
	if cause != nil {
		return failed(response, publicSupervisorError("restart custody could not be established; no restart was requested", cause), 1)
	}
	response.SupervisorControl.State = "restart_preflight"
	if pending {
		response.SupervisorControl.State = "restart_pending"
	}
	response.SupervisorControl.NextAction = runtimeRestartPreflightNextAction(observed.Custody, pending)
	return commandResult{response: response}
}

func runtimeRestartPreflightNextAction(custody infrastructure.RuntimeRestartCustody, pending bool) string {
	if pending {
		return "repeat_original_restart"
	}
	if custody == infrastructure.RuntimeRestartCustodyAvailable {
		return "explicit_restart"
	}
	return "stop_and_report"
}

// The host owns store and its cleanup. Control lends the actual bootstrap EX
// and complete queue snapshot; only its journal may enable the fenced scan.
// Infrastructure separately owns current-startup and native process authority.
func authorizePrivateRuntimeRestart(ctx context.Context, store *taskdir.Store, from pueue.RuntimeRestartReference, binding task.SupervisorRef, jobs []pueue.Job) error {
	if ctx == nil || store == nil {
		return os.ErrInvalid
	}
	if err := pueue.RequireRuntimeRestartBootstrap(ctx, store.Root); err != nil {
		return err
	}
	if err := validateRuntimeRestartSubject(ctx, store, from); err != nil {
		return err
	}
	pending, err := pueue.PrivateRuntimeRestartPending(ctx, store.Root)
	if err != nil {
		return err
	}
	if !pending {
		return supervisor.CheckRuntimeQuiescence(ctx, store, binding, jobs)
	}
	observed, err := pueue.ObservePrivateRuntimeRestart(ctx, store.Root, from)
	if err != nil {
		return err
	}
	if observed.From != from || observed.Binding != binding || observed.Phase == pueue.RuntimeRestartReady {
		return task.ErrIdentityMismatch
	}
	return supervisor.CheckFencedRuntimeQuiescence(ctx, store, binding, jobs)
}

func validateRuntimeRestartSubject(ctx context.Context, store *taskdir.Store, from pueue.RuntimeRestartReference) (resultErr error) {
	reference, err := infrastructure.ParseRuntimeRestartReference(string(from))
	if err != nil {
		return err
	}
	if reference.RootID != store.RootID {
		return task.ErrIdentityMismatch
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	return infrastructure.ValidateRuntimeRestartReference(ctx, reader, reference)
}
