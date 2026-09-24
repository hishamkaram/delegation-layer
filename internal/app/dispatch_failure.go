package app

import (
	"errors"

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

func dispatchFailureMessage(code string) string {
	switch code {
	case "invalid_request":
		return "Delegate could not accept the dispatch request."
	case "unsupported_option":
		return "The selected provider does not support one of the requested options."
	case "task_request_conflict":
		return "The dispatch request conflicts with an existing immutable task."
	case "task_identity_mismatch":
		return "Delegate could not verify the saved task identity."
	case "supervisor_setup_failed":
		return "Delegate could not prepare its private task supervisor."
	case "provider_preflight_failed":
		return "Delegate could not complete provider preflight."
	case "delegate_installation_incomplete":
		return "The Delegation Layer installation is incomplete."
	case "provider_unavailable":
		return "The selected provider is not available in this environment."
	case "task_submission_uncertain":
		return "Delegate created the task record but could not confirm submission. Check its status before taking further action."
	case "task_submission_preparation_failed":
		return "Delegate created the task record but could not prepare it for submission. Check its status before taking further action."
	case "task_operation_failed":
		return "Delegate could not safely inspect or reuse the saved task. Check its status before taking further action."
	case "state_root_unavailable":
		return "Delegate could not access the saved task state. Stop and report this failure."
	case "continuation_unavailable":
		return "Delegate could not load or validate the predecessor for continuation. Stop and report this failure."
	case "unsupported_continuation":
		return "The selected provider does not support resumable continuation. Stop and report this limitation."
	default:
		return "Delegate could not complete dispatch."
	}
}
