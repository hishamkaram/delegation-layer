package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type dispatchStageError struct {
	stage string
	err   error
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
	return DispatchFailureResponse{
		Code:       code,
		Stage:      stage,
		NextAction: dispatchFailureNextAction(code, taskRecord),
	}
}

func dispatchFailureCode(err error, stage, taskRecord string) string {
	switch {
	case stage == "submit-task" && taskRecord == "created" && errors.Is(err, pueue.ErrSubmissionUncertain):
		return "task_submission_uncertain"
	case errors.Is(err, commonprovider.ErrUnsupportedOption):
		return "unsupported_option"
	case errors.Is(err, task.ErrRequestConflict):
		return "task_request_conflict"
	case errors.Is(err, task.ErrIdentityMismatch):
		return "task_identity_mismatch"
	case errors.Is(err, inspection.ErrGroupMigrationBusy):
		return "supervisor_migration_blocked"
	case errors.Is(err, inspection.ErrGroupMigrationRequired):
		return "supervisor_upgrade_required"
	case errors.Is(err, ErrProfileUnavailable):
		return "provider_unavailable"
	}
	if code := dispatchFailureStageCode(stage); code != "" {
		return code
	}
	if taskRecord == "created" {
		return "task_operation_failed"
	}
	return "dispatch_failed"
}

func dispatchFailureStageCode(stage string) string {
	switch stage {
	case "validate-request":
		return "invalid_request"
	case "prepare-provider", "finalize-provider", "select-provider", "validate-profile", "record-runtime-capability":
		return "provider_preparation_failed"
	case "prepare-supervisor-options", "start-supervisor":
		return "supervisor_setup_failed"
	case "inspect-provider":
		return "provider_preflight_failed"
	case "prepare-task-submission":
		return "task_submission_preparation_failed"
	case "resolve-runner":
		return "delegate_installation_incomplete"
	case "resolve-state-root":
		return "state_root_unavailable"
	case "resolve-predecessor", "load-predecessor", "validate-continuation", "restore-continuation-environment":
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
	if taskRecord == "created" {
		return "check_status"
	}
	if taskRecord == "not_created" && (code == "invalid_request" || code == "unsupported_option") {
		return "correct_request"
	}
	return "stop_and_report"
}

var dispatchFailureMessages = map[string]string{
	"invalid_request":                    "Delegate could not accept the dispatch request.",
	"unsupported_option":                 "The selected provider does not support one of the requested options.",
	"task_request_conflict":              "The dispatch request conflicts with an existing immutable task.",
	"task_identity_mismatch":             "Delegate could not verify the saved task identity.",
	"supervisor_migration_blocked":       "Delegate found active inspection work on an older supervisor and could not migrate it safely.",
	"supervisor_upgrade_required":        "Delegate found an older inspection supervisor binding that requires safe migration before dispatch.",
	"supervisor_setup_failed":            "Delegate could not prepare its private task supervisor.",
	"provider_preflight_failed":          "Delegate could not complete provider preflight.",
	"delegate_installation_incomplete":   "The Delegation Layer installation is incomplete.",
	"provider_unavailable":               "The selected provider is not available in this environment.",
	"task_submission_uncertain":          "Delegate created the task record but could not confirm submission. Check its status before taking further action.",
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
