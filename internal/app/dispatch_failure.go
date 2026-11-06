package app

import (
	"context"
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func dispatchFailure(err error, taskRecord string) DispatchFailureResponse {
	stage := "dispatch"
	var staged *admissioncore.StageError
	if errors.As(err, &staged) {
		stage = staged.Stage()
	}
	if teamDispatchStage(stage) && !errors.Is(err, task.ErrRequestConflict) {
		failure := teamDispatchFailure(err)
		failure.Stage = stage
		return failure
	}

	code := dispatchFailureCode(err, stage, taskRecord)
	failure := DispatchFailureResponse{
		Code:            code,
		Stage:           stage,
		NextAction:      dispatchFailureNextAction(code, taskRecord),
		RecoveryCommand: dispatchFailureRecoveryCommand(code, taskRecord),
	}
	if supervisorFailureCode(code) {
		failure.ReasonCode = supervisorFailureReason(err)
		if taskRecord == "not_created" && failure.ReasonCode == "historical_target_not_ready" {
			failure.NextAction = "stop_and_report"
			failure.RecoveryCommand = ""
		}
	}
	if code == "invalid_request" {
		failure.ReasonCode = runnerRequestReason(err)
	}
	if pressure, ok := tasklifecycle.ResourcePressureRefusal(err); ok {
		failure.ReasonCode = resourcePressureReason(pressure)
	}
	if code == "provider_unavailable" && commonprovider.NativeCapabilityUnavailable(err) {
		failure.ReasonCode = providerCapabilityChangedReason
	}
	if failure.NextAction == "retry_later" {
		failure.RetryAfterSeconds = resourceRetryAfterSeconds
	}
	if stage == "inspect-provider" {
		failure.Inspection = inspectionFailureResponse(err)
	}
	return failure
}

// providerCapabilityChangedReason marks an unavailable provider whose whole
// refusal is a capability the recorded launch requires and the installed CLI
// no longer offers. Its public detail is fixed and names no provider value.
const providerCapabilityChangedReason = "provider_capability_changed"

const providerCapabilityChangedDetail = "The installed provider CLI no longer offers a capability that this task's recorded launch requires."

// Reason codes for a request whose --runner contradicts the runner ownership
// its command needs. Each marks a whole refusal by one exact owned leaf.
const (
	managedWorkspaceRunnerReason = "managed_workspace_runner_conflict"
	continuationRunnerReason     = "continuation_runner_conflict"
)

// runnerRequestReason reports the reason code of an error whose every leaf is
// one exact runner-request refusal, or "" for any other error. A joined
// storage, evidence, identity, or close fault fails the whole predicate.
func runnerRequestReason(err error) string {
	switch {
	case task.AllErrorCauses(err, workspaces.IsManagedRunnerRequest):
		return managedWorkspaceRunnerReason
	case task.AllErrorCauses(err, supervisorcore.IsContinuationRunnerRequest):
		return continuationRunnerReason
	default:
		return ""
	}
}

// runnerRequestDetail is the fixed correction guidance for a runner-request
// reason code, or "" for any other reason.
func runnerRequestDetail(reason string) string {
	switch reason {
	case managedWorkspaceRunnerReason:
		return "A managed workspace uses Delegate's automatic runner; omit --runner."
	case continuationRunnerReason:
		return "An explicit continuation runner must match the predecessor's recorded runner; omit --runner to select the runner automatically."
	default:
		return ""
	}
}

// fixedDetailPublicError presents a refusal with its fixed detail while
// keeping the complete command error as its cause.
type fixedDetailPublicError struct {
	message string
	cause   error
}

func (e *fixedDetailPublicError) Error() string { return e.message }

func (e *fixedDetailPublicError) Unwrap() error { return e.cause }

// supervisorFailureCode reports the failure codes whose reason codes come
// from supervisorFailureReason and whose detail is supervisor-specific.
func supervisorFailureCode(code string) bool {
	return code == "supervisor_owner_incompatible" || code == "supervisor_upgrade_required" || code == "supervisor_migration_blocked" || code == "supervisor_setup_failed"
}

func inspectionFailureResponse(err error) *InspectionFailureResponse {
	response := &InspectionFailureResponse{
		Class: inspectionFailureClass(err), DeadlineState: "unknown",
	}
	var contextual *admissioncore.InspectionFailureContextError
	if errors.As(err, &contextual) {
		response.DeadlineState = contextual.DeadlineState()
		response.JournalTaskID = contextual.TaskID()
	}
	return response
}

func inspectionFailureClass(err error) string {
	switch {
	case errors.Is(err, inspection.ErrAdmissionExpired):
		return "admission_deadline_expired"
	case errors.Is(err, admissioncore.ErrInspectionWorkerUnavailable):
		return "inspection_worker_failed"
	case errors.Is(err, ErrProfileUnavailable):
		return "provider_unavailable"
	case errors.Is(err, task.ErrEvidenceFault), errors.Is(err, task.ErrIdentityMismatch):
		return "inspection_evidence_invalid"
	case errors.Is(err, task.ErrLockBusy):
		return "inspection_journal_contended"
	case errors.Is(err, pueue.ErrStatusOperational), errors.Is(err, pueue.ErrControlLimit),
		errors.Is(err, pueue.ErrUnknown), errors.Is(err, pueue.ErrInFlight):
		return "supervisor_observation_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return "observation_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "observation_canceled"
	default:
		return "inspection_failed"
	}
}

func dispatchFailureCode(err error, stage, taskRecord string) string {
	if _, ok := tasklifecycle.ResourcePressureRefusal(err); ok {
		return "resource_pressure"
	}
	if code := dispatchSubmissionFailureCode(err, stage, taskRecord); code != "" {
		return code
	}
	if code := dispatchCauseFailureCode(err); code != "" {
		return code
	}
	if code := dispatchFailureStageCode(stage); code != "" {
		return code
	}
	if taskRecord == "created" {
		return "task_operation_failed"
	}
	return "dispatch_failed"
}

func dispatchSubmissionFailureCode(err error, stage, taskRecord string) string {
	if taskRecord != "created" {
		return ""
	}
	switch {
	case stage == "submit-task" && errors.Is(err, pueue.ErrSubmissionUncertain):
		return "task_submission_uncertain"
	case stage == "enqueue-task" && errors.Is(err, pueue.ErrEnqueueUncertain):
		return "task_launch_uncertain"
	default:
		return ""
	}
}

func dispatchCauseFailureCode(err error) string {
	if runnerRequestReason(err) != "" {
		return "invalid_request"
	}
	if code := workspaceFailureCode(err); code != "" {
		return code
	}
	switch {
	case errors.Is(err, infrastructure.ErrOwnerIncompatible):
		return "supervisor_owner_incompatible"
	case errors.Is(err, commonprovider.ErrUnsupportedOption):
		return "unsupported_option"
	case errors.Is(err, task.ErrRequestConflict):
		return "task_request_conflict"
	case errors.Is(err, task.ErrIdentityMismatch):
		return "task_identity_mismatch"
	case errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy):
		return "supervisor_migration_blocked"
	case errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired):
		return "supervisor_upgrade_required"
	case errors.Is(err, inspection.ErrGroupMigrationBusy):
		return "supervisor_migration_blocked"
	case errors.Is(err, inspection.ErrGroupMigrationUnsupported):
		return "supervisor_migration_unsupported"
	case errors.Is(err, inspection.ErrGroupMigrationRequired):
		return "supervisor_upgrade_required"
	case errors.Is(err, ErrProfileUnavailable):
		return "provider_unavailable"
	default:
		return ""
	}
}

func workspaceFailureCode(err error) string {
	switch {
	case errors.Is(err, workspace.ErrReleased):
		return "workspace_released"
	case workspaces.PendingError(err):
		return "workspace_busy"
	case errors.Is(err, workspace.ErrIdentity), errors.Is(err, workspace.ErrIdentityMismatch):
		return "workspace_identity_changed"
	default:
		return ""
	}
}

func dispatchFailureStageCode(stage string) string {
	switch stage {
	case "validate-request":
		return "invalid_request"
	case "prepare-provider", "finalize-provider", "select-provider", "validate-profile", "record-runtime-capability":
		return "provider_preparation_failed"
	case "prepare-supervisor-executables", "prepare-supervisor-options", "start-supervisor":
		return "supervisor_setup_failed"
	case "inspect-provider":
		return "provider_preflight_failed"
	case "prepare-task-submission":
		return "task_submission_preparation_failed"
	case "resolve-runner":
		return "delegate_installation_incomplete"
	case "resolve-state-root":
		return "state_root_unavailable"
	case "prepare-workspace":
		return "workspace_preparation_failed"
	case "resolve-predecessor", "load-predecessor", "load-continuation-session", "validate-continuation", "resolve-continuation-supervisor":
		return "continuation_unavailable"
	case "resolve-continuation-provider":
		return "provider_unavailable"
	case "check-continuation-capability":
		return "unsupported_continuation"
	default:
		return ""
	}
}

func dispatchFailureNextAction(code, taskRecord string) string {
	if code == "supervisor_owner_incompatible" {
		return "stop_and_report"
	}
	if taskRecord == "not_created" && code == "resource_pressure" {
		return "retry_later"
	}
	if taskRecord == "not_created" && code == "supervisor_upgrade_required" {
		return "run_supervisor_migrate"
	}
	if taskRecord == "not_created" && code == "supervisor_migration_blocked" {
		return "wait_for_legacy_work"
	}
	if taskRecord == "created" {
		return "check_status"
	}
	if taskRecord == "not_created" && (code == "invalid_request" || code == "unsupported_option") {
		return "correct_request"
	}
	return "stop_and_report"
}

func dispatchFailureRecoveryCommand(code, taskRecord string) string {
	if code != "supervisor_upgrade_required" || taskRecord != "not_created" {
		return ""
	}
	return "delegate supervisor migrate --root <same-state-root> --json"
}

var dispatchFailureMessages = map[string]string{
	"team_selection_invalid":             "The requested Team or Member cannot be selected. Inspect its scope, project and current definition; correct the request before trying again.",
	"team_catalog_unavailable":           "Delegate could not read the Team catalog or verify its project context. Inspect the catalog and project before retrying; no replacement Member was selected.",
	"invalid_request":                    "Delegate could not accept the dispatch request.",
	"unsupported_option":                 "The selected provider does not support one of the requested options.",
	"task_request_conflict":              "The dispatch request conflicts with an existing immutable task.",
	"task_identity_mismatch":             "Delegate could not verify the saved task identity.",
	"supervisor_migration_blocked":       "Delegate found active work on an older private supervisor and could not migrate it safely.",
	"supervisor_migration_unsupported":   "Delegate found an external supervisor binding that it must not stop or migrate.",
	"supervisor_owner_incompatible":      "Delegate's running infrastructure owner does not match the required installation; state was preserved.",
	"supervisor_upgrade_required":        "Delegate found supervisor state that requires safe migration before dispatch.",
	"supervisor_setup_failed":            "Delegate could not prepare its private task supervisor.",
	"provider_preflight_failed":          "Delegate could not complete provider preflight.",
	"delegate_installation_incomplete":   "The Delegation Layer installation is incomplete.",
	"provider_unavailable":               "The selected provider is not available in this environment.",
	"task_submission_uncertain":          "Delegate created the task record but could not confirm submission. Check its status before taking further action.",
	"task_launch_uncertain":              "Delegate recorded the exact supervisor task but could not confirm that it was enqueued. Check its status to resume the staged launch.",
	"task_submission_preparation_failed": "Delegate created the task record but could not prepare it for submission. Check its status before taking further action.",
	"task_operation_failed":              "Delegate could not safely inspect or reuse the saved task. Check its status before taking further action.",
	"state_root_unavailable":             "Delegate could not access the saved task state. Stop and report this failure.",
	"continuation_unavailable":           "Delegate could not load or validate the predecessor for continuation. Stop and report this failure.",
	"unsupported_continuation":           "The selected provider does not support resumable continuation. Stop and report this limitation.",
	"workspace_busy":                     "The managed workspace is still in use or awaiting capture. Observe its workspace status.",
	"workspace_provision_pending":        "Delegate is preparing the managed workspace. Observe its workspace status before retrying the same request.",
	"workspace_released":                 "The managed workspace was released and cannot accept another turn.",
	"workspace_identity_changed":         "Delegate could not verify the managed workspace identity. Its files were retained.",
	"workspace_preparation_failed":       "Delegate could not prepare the managed workspace. Stop and report this failure; its saved evidence was retained.",
	"resource_pressure":                  "Delegate refused new work because resource pressure was confirmed.",
}

// genericFailureMessage is the neutral public text for a failure code without a
// specific message. It names no command, so every command may present it.
const genericFailureMessage = "Delegate could not complete the operation."

func dispatchFailureMessage(code string) string {
	if message, ok := dispatchFailureMessages[code]; ok {
		return message
	}
	return genericFailureMessage
}
