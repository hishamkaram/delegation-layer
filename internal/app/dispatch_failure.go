package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type dispatchStageError struct {
	stage string
	err   error
}

type inspectionFailureContextError struct {
	err           error
	taskID        string
	deadlineState string
}

func (e *inspectionFailureContextError) Error() string { return e.err.Error() }

func (e *inspectionFailureContextError) Unwrap() error { return e.err }

func withInspectionFailureContext(err error, taskID string, deadline time.Time) error {
	if err == nil {
		return nil
	}
	state := "unknown"
	if !deadline.IsZero() {
		state = "open"
		if !time.Now().Before(deadline) {
			state = "expired"
		}
	}
	if errors.Is(err, inspection.ErrAdmissionExpired) {
		state = "expired"
	}
	return &inspectionFailureContextError{err: err, taskID: taskID, deadlineState: state}
}

func (e *dispatchStageError) Error() string { return e.err.Error() }

func (e *dispatchStageError) Unwrap() error { return e.err }

func withDispatchStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var staged *dispatchStageError
	if errors.As(err, &staged) {
		return err
	}
	return &dispatchStageError{stage: stage, err: err}
}

func dispatchFailure(err error, taskRecord string) DispatchFailureResponse {
	stage := "dispatch"
	var staged *dispatchStageError
	if errors.As(err, &staged) {
		stage = staged.stage
	}

	code := dispatchFailureCode(err, stage, taskRecord)
	failure := DispatchFailureResponse{
		Code:            code,
		Stage:           stage,
		NextAction:      dispatchFailureNextAction(code, taskRecord),
		RecoveryCommand: dispatchFailureRecoveryCommand(code, taskRecord),
	}
	if stage == "inspect-provider" {
		failure.Inspection = inspectionFailureResponse(err)
	}
	return failure
}

func inspectionFailureResponse(err error) *InspectionFailureResponse {
	response := &InspectionFailureResponse{
		Class: inspectionFailureClass(err), DeadlineState: "unknown",
	}
	var contextual *inspectionFailureContextError
	if errors.As(err, &contextual) {
		response.DeadlineState = contextual.deadlineState
		response.JournalTaskID = contextual.taskID
	}
	return response
}

func inspectionFailureClass(err error) string {
	switch {
	case errors.Is(err, inspection.ErrAdmissionExpired):
		return "admission_deadline_expired"
	case errors.Is(err, errInspectionWorkerUnavailable):
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
	switch {
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
	case "resolve-predecessor", "load-predecessor", "validate-continuation", "resolve-continuation-supervisor", "restore-continuation-environment":
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
	"invalid_request":                    "Delegate could not accept the dispatch request.",
	"unsupported_option":                 "The selected provider does not support one of the requested options.",
	"task_request_conflict":              "The dispatch request conflicts with an existing immutable task.",
	"task_identity_mismatch":             "Delegate could not verify the saved task identity.",
	"supervisor_migration_blocked":       "Delegate found active work on an older private supervisor and could not migrate it safely.",
	"supervisor_migration_unsupported":   "Delegate found an external supervisor binding that it must not stop or migrate.",
	"supervisor_upgrade_required":        "Delegate found an older private supervisor binding that requires safe migration before dispatch.",
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
}

func dispatchFailureMessage(code string) string {
	if message, ok := dispatchFailureMessages[code]; ok {
		return message
	}
	return "Delegate could not complete dispatch."
}
