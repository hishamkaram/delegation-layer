package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

type supervisorPublicError struct {
	message string
	cause   error
}

func (e *supervisorPublicError) Error() string { return e.message }

func (e *supervisorPublicError) Unwrap() error { return e.cause }

func publicSupervisorError(message string, err error) error {
	if detail := supervisorFailureDetail(supervisorFailureReason(err)); detail != "" {
		message += " " + detail
	}
	return &supervisorPublicError{message: message, cause: err}
}

func commandPresentationError(command string, result commandResult) error {
	if command == "dispatch" || command == "continue" {
		if public := dispatchPublicError(dispatchFailure(result.err, result.response.TaskRecord), result.err); public != nil {
			return public
		}
	}
	return result.err
}

// dispatchPublicError is the detailed public error for a failure whose code
// has a typed detail, or nil when the command error is presented unchanged.
func dispatchPublicError(failure DispatchFailureResponse, err error) error {
	switch {
	case failure.Code == "resource_pressure":
		return publicResourceError(dispatchFailureMessage(failure.Code), err)
	case supervisorFailureCode(failure.Code) && failure.ReasonCode != "":
		return publicSupervisorError(dispatchFailureMessage(failure.Code), err)
	case failure.Code == "invalid_request" && runnerRequestDetail(failure.ReasonCode) != "":
		return &fixedDetailPublicError{message: dispatchFailureMessage(failure.Code) + " " + runnerRequestDetail(failure.ReasonCode), cause: err}
	case failure.ReasonCode == providerCapabilityChangedReason:
		return &fixedDetailPublicError{message: dispatchFailureMessage(failure.Code) + " " + providerCapabilityChangedDetail, cause: err}
	default:
		return nil
	}
}

func supervisorFailureReason(err error) string {
	var recovery *pueue.PrivateRecoveryError
	if errors.As(err, &recovery) && supervisorFailureDetail(recovery.ReasonCode) != "" {
		return recovery.ReasonCode
	}
	switch {
	case errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy), errors.Is(err, inspection.ErrGroupMigrationBusy):
		return "active_work"
	case errors.Is(err, pueue.ErrInFlight), errors.Is(err, pueue.ErrStatusOperational), errors.Is(err, pueue.ErrControlLimit),
		errors.Is(err, pueue.ErrUnknown), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "observation_failed"
	default:
		return ""
	}
}

func supervisorFailureDetail(reason string) string {
	switch reason {
	case "identity_conflict":
		return "Saved supervisor identities conflict; state was preserved."
	case "transition_pending":
		return "A saved supervisor transition must finish before admission."
	case "published_pair_unavailable_live":
		return "The published executables are unavailable while their daemon is live; run supervisor migrate with the same root."
	case "predecessor_pair_unavailable":
		return "The exact predecessor executable pair is unavailable; use supervisor migrate with the same root."
	case "historical_target_not_ready":
		return "The historical supervisor is not ready. Automatic migration of a stopped historical generation is unavailable; state was preserved."
	case "active_work":
		return "Wait for queued or running work before migrating."
	case "observation_failed":
		return "Supervisor observation could not establish a safe recovery action."
	case "state_unreadable":
		return "Supervisor state could not be validated; state was preserved."
	case "endpoint_owner_unknown":
		return "The supervisor endpoint refuses connections and its recorded process is unknown; state was preserved."
	case "endpoint_refused_live":
		return "The supervisor endpoint refuses connections while its recorded process is present; state was preserved."
	default:
		return ""
	}
}
