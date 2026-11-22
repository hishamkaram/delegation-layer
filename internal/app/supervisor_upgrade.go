package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// SupervisorUpgradeResponse is bounded control metadata. The canonical journal
// retains full predecessor/target bindings; public output never exposes them.
type SupervisorUpgradeResponse struct {
	OperationID       string   `json:"operation_id"`
	Mode              string   `json:"mode"`
	Phase             string   `json:"phase"`
	Completed         bool     `json:"completed,omitempty"`
	CompletionCommand []string `json:"completion_command,omitempty"`
}

func supervisorEnsureOwner(a Arguments, deps Dependencies) (result commandResult) {
	return supervisorEnsureOwnerContext(context.Background(), a, deps.withCapturedEnvironment(), "")
}

func supervisorEnsureOwnerContext(parent context.Context, a Arguments, deps Dependencies, expectedRoot string) (result commandResult) {
	response := newSupervisorCommandResponse("ensure-owner")
	if parent == nil {
		return failedSupervisorCause(response, "supervisor upgrade context is unavailable", os.ErrInvalid, 1)
	}
	options := supervisorControlOptions(deps)
	ctx, cancel := supervisor.ControlContextWithParent(parent, options)
	defer cancel()
	if err := deps.invocationSource.ValidateUpgrade(ctx); err != nil {
		return failedSupervisorCause(response, "supervisor upgrade invocation is unavailable", err, 1)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedSupervisorCause(response, "supervisor root is unavailable", err, 1)
	}
	plain := expectedRoot == "" && !a.SupervisorPrepareUpgrade && a.SupervisorCompleteUpgrade == "" && !a.SupervisorRefreshOwner
	store, bootstrap, err := openSupervisorUpgradeStore(ctx, root, deps, plain)
	if err != nil {
		return failedSupervisorCause(response, "upgrade requires a valid existing root", err, 1)
	}
	if bootstrap {
		return supervisorBootstrapOwner(ctx, a, deps)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	if expectedRoot != "" {
		observed, tokenErr := store.RootTokenContext(ctx)
		if tokenErr != nil || observed != expectedRoot {
			return failedSupervisorCause(response, "supervisor root identity changed", errors.Join(task.ErrIdentityMismatch, tokenErr), 1)
		}
	}
	return supervisorEnsureOpened(ctx, a, deps, store)
}

func supervisorEnsureOpened(ctx context.Context, a Arguments, deps Dependencies, store *taskdir.Store) commandResult {
	request := infrastructure.UpgradeRequest{Action: infrastructure.UpgradeEnsure}
	if a.SupervisorRefreshOwner && !a.SupervisorPrepareUpgrade && a.SupervisorCompleteUpgrade == "" {
		request.Action = infrastructure.UpgradeStatus
	}
	if a.SupervisorPrepareUpgrade {
		request.Action = infrastructure.UpgradePrepare
	}
	if a.SupervisorCompleteUpgrade != "" {
		request.Action = infrastructure.UpgradeComplete
		request.OperationID = infrastructure.UpgradeOperationID(a.SupervisorCompleteUpgrade)
	}
	return supervisorEnsureRequestOpened(ctx, a, deps, store, request, a.SupervisorRefreshOwner)
}

// The Gateway bridge selects an explicit owner intent after validating the
// same opened root. Public refresh-only arguments retain their Status meaning.
func supervisorEnsureRequestOpened(ctx context.Context, a Arguments, deps Dependencies, store *taskdir.Store, request infrastructure.UpgradeRequest, refresh bool) commandResult {
	response := newSupervisorCommandResponse("ensure-owner")
	response.RootID = store.RootID
	options := supervisorControlOptions(deps)
	if request.Action == infrastructure.UpgradeComplete {
		observed, done, observeErr := observeSupervisorUpgradeCompletion(ctx, store, request.OperationID)
		if done || observeErr != nil {
			return finishSupervisorUpgrade(a, response, observed, observeErr)
		}
	}
	control := supervisor.ControlRuntime{Options: options, Lookup: deps.supervisorLookup(), Routing: deps.infrastructureRouting()}
	outcome, err := control.UpgradeOwn(ctx, store, request, refresh)
	result := finishSupervisorUpgrade(a, response, outcome, err)
	if outcome.Report.Phase == infrastructure.UpgradeDraining {
		diagnostic := supervisorMaintenanceDiagnostic(ctx, store, deps, options)
		result = finishSupervisorUpgradeDiagnostic(result, diagnostic)
	}
	return result
}

func finishSupervisorUpgradeDiagnostic(result, diagnostic commandResult) commandResult {
	control := result.response.SupervisorControl
	attachSupervisorDiagnostic(control, diagnostic.response.SupervisorControl)
	if diagnostic.err != nil {
		result.code = 1
		result.err = publicSupervisorError("supervisor upgrade diagnostic failed", errors.Join(result.err, diagnostic.err))
		control.NextAction = "stop_and_report"
	} else if result.code == 3 && result.err == nil && !upgradeStartRequiresReview(control.ReasonCode) {
		applyMaintenanceGuidance(control)
	}
	return result
}

func openSupervisorUpgradeStore(ctx context.Context, root string, deps Dependencies, plain bool) (*taskdir.Store, bool, error) {
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, root, deps.storeDependencies().registry())
	if supervisorRootAbsent(root, err) && plain {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !plain {
		return store, false, nil
	}
	fresh, err := supervisorBootstrapAbsent(root)
	if err != nil || fresh {
		return nil, fresh, errors.Join(err, store.Close())
	}
	return store, false, nil
}

func observeActiveSupervisorUpgrade(ctx context.Context, store *taskdir.Store) (pueue.PrivateUpgradeObservation, bool, error) {
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return pueue.PrivateUpgradeObservation{}, false, err
	}
	return pueue.ObservePrivateUpgrade(ctx, pin, "")
}

func supervisorMaintenanceClient(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, clientExecutable, daemonExecutable string, options pueue.Options) (*pueue.Client, error) {
	observed, found, err := observeActiveSupervisorUpgrade(ctx, store)
	if err != nil {
		return nil, err
	}
	if found {
		return pueue.ProbeSavedPrivateUpgradeControl(ctx, store.Root, observed.Predecessor, options, func(context.Context) (string, string, error) {
			return clientExecutable, daemonExecutable, nil
		})
	}
	client, err := pueue.BindSavedForMigrationWithClient(ctx, binding, clientExecutable, options)
	// Only the optional upgrade probe can report clean transport absence.
	if client == nil && err == nil {
		return nil, task.ErrEvidenceFault
	}
	return client, err
}

func supervisorUpgradeStatus(ctx context.Context, store *taskdir.Store, deps Dependencies, options pueue.Options, response Response, observed pueue.PrivateUpgradeObservation) commandResult {
	control := response.SupervisorControl
	control.State = "blocked"
	control.MigrationPhase = string(observed.Phase)
	control.Upgrade = &SupervisorUpgradeResponse{OperationID: string(observed.OperationID), Mode: string(observed.Mode), Phase: string(observed.Phase)}
	control.NextAction = "run_supervisor_ensure_owner"
	control.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if observed.Phase == pueue.UpgradeAwaitingInstallation {
		control.NextAction = "install_then_complete_upgrade"
	}
	if observed.Phase != pueue.UpgradeDraining {
		return commandResult{response: response}
	}
	diagnostic := supervisorMaintenanceDiagnostic(ctx, store, deps, options)
	attachSupervisorDiagnostic(control, diagnostic.response.SupervisorControl)
	if diagnostic.err != nil {
		control.NextAction = "stop_and_report"
		return failedSupervisorCause(response, "supervisor maintenance observation failed", diagnostic.err, 1)
	}
	applyMaintenanceGuidance(control)
	return commandResult{response: response}
}

func supervisorBootstrapAbsent(root string) (bool, error) {
	eligible, err := pueue.PrivateSupervisorInitialBootstrapEligible(root)
	if err != nil || eligible {
		return eligible, err
	}
	_, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil || found {
		return false, err
	}
	return false, pueue.ErrPrivateSupervisorTransitionIncomplete
}

func observeSupervisorUpgradeCompletion(ctx context.Context, store *taskdir.Store, operation infrastructure.UpgradeOperationID) (infrastructure.UpgradeOutcome, bool, error) {
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return infrastructure.UpgradeOutcome{}, true, err
	}
	op, err := pueue.ParseUpgradeOperationID(string(operation))
	if err != nil {
		return infrastructure.UpgradeOutcome{}, true, err
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, op)
	if err != nil || !found {
		return infrastructure.UpgradeOutcome{}, true, errors.Join(pueue.ErrPrivateUpgradeNotRetained, err)
	}
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{OperationID: operation, Mode: infrastructure.UpgradeMode(observed.Mode), Phase: infrastructure.UpgradePhase(observed.Phase), Predecessor: observed.Predecessor, Completed: observed.Completed}}
	return outcome, observed.Completed, outcome.Validate()
}

func finishSupervisorUpgrade(a Arguments, response Response, outcome infrastructure.UpgradeOutcome, cause error) commandResult {
	control := response.SupervisorControl
	control.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	control.NextAction = "stop_and_report"
	validationErr := outcome.Validate()
	if validationErr == nil {
		control.ReasonCode = outcome.ReasonCode
		cause = supervisorUpgradeFactCause(outcome.FailureFact, cause)
	}
	if validationErr == nil && outcome.Report != (infrastructure.UpgradeReport{}) {
		report := outcome.Report
		control.Upgrade = &SupervisorUpgradeResponse{OperationID: string(report.OperationID), Mode: string(report.Mode), Phase: string(report.Phase), Completed: report.Completed}
		control.MigrationPhase = string(report.Phase)
	}
	if err := errors.Join(cause, validationErr); err != nil {
		return failedSupervisorCause(response, "supervisor upgrade could not be completed", err, 1)
	}
	if outcome == (infrastructure.UpgradeOutcome{}) {
		return failedSupervisorCause(response, "supervisor upgrade returned no verified observation", task.ErrEvidenceFault, 1)
	}
	control.State = "blocked"
	switch outcome.Condition {
	case infrastructure.UpgradePending, infrastructure.UpgradeBusy:
		control.NextAction = "repeat_supervisor_ensure_owner"
		if upgradeStartRequiresReview(outcome.ReasonCode) {
			control.NextAction = "stop_and_report"
		} else if strings.HasPrefix(outcome.ReasonCode, "ordinary_migration_") {
			control.NextAction = "run_supervisor_migrate"
		}
		return commandResult{response: response, code: 3}
	case infrastructure.UpgradeRefused:
		return failedSupervisor(response, "supervisor upgrade was refused", 1)
	case infrastructure.UpgradeUnsupported:
		return failedSupervisor(response, "supervisor upgrade is unsupported", 1)
	case infrastructure.UpgradeFault:
		return failedSupervisor(response, "supervisor upgrade failed", 1)
	case infrastructure.UpgradeTimeout:
		return failedSupervisor(response, "supervisor upgrade timed out", 1)
	case infrastructure.UpgradeOK:
	}
	return finishSuccessfulSupervisorUpgrade(a, response, outcome)
}

// Accepted-but-uncertain or exhausted native Starts have no replay authority.
// A later queue diagnostic cannot turn that retained operation into a retry.
func upgradeStartRequiresReview(reason string) bool {
	switch reason {
	case "predecessor_birth_unknown", "predecessor_boot_changed", "predecessor_boot_unknown", "predecessor_wait_closed", "predecessor_not_ready", "predecessor_pid_mismatch", "predecessor_pair_missing",
		"successor_birth_unknown", "successor_boot_changed", "successor_wait_closed", "successor_pid_mismatch", "successor_not_ready", infrastructurehost.UpgradeOwnerRefreshSuccessorStartAccepted:
		return true
	default:
		return false
	}
}

func finishSuccessfulSupervisorUpgrade(a Arguments, response Response, outcome infrastructure.UpgradeOutcome) commandResult {
	control := response.SupervisorControl
	if outcome.Report == (infrastructure.UpgradeReport{}) {
		if a.SupervisorRefreshOwner && !a.SupervisorPrepareUpgrade && a.SupervisorCompleteUpgrade == "" {
			control.State = "helper_refreshed"
			control.NextAction = "inspect_supervisor_status"
			return commandResult{response: response}
		}
		if outcome.Disposition != (infrastructure.Disposition{}) {
			response.Status = "owner_ready"
			control.State = "ready"
			control.NextAction = "retry_dispatch"
			return commandResult{response: response}
		}
	}
	if outcome.Report.Phase == infrastructure.UpgradeAwaitingInstallation {
		control.State = "upgrade_prepared"
		control.NextAction = "install_then_complete_upgrade"
		control.Upgrade.CompletionCommand = supervisorUpgradeCompletionCommand(a, string(outcome.Report.OperationID))
		if !a.SupervisorPrepareUpgrade {
			return commandResult{response: response, code: 3}
		}
		return commandResult{response: response}
	}
	if outcome.Report.Completed {
		control.State = "migrated"
		control.NextAction = "inspect_supervisor_status"
		if outcome.Disposition != (infrastructure.Disposition{}) {
			response.Status = "owner_ready"
			control.NextAction = "retry_dispatch"
		}
		return commandResult{response: response}
	}
	return failedSupervisor(response, "supervisor upgrade returned incomplete progress", 1)
}

func supervisorUpgradeCompletionCommand(a Arguments, operation string) []string {
	argv := []string{"delegate"}
	if a.Root != "" {
		argv = append(argv, "--root", a.Root)
	}
	return append(argv, "supervisor", "ensure-owner", "--complete-upgrade", operation, "--refresh-owner", "--json")
}
