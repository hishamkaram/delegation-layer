package task

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

// SessionIdentity is an observed provider conversation, never a launch instruction.
type (
	SessionIdentity struct {
		Provider       string
		ConversationID string
	}
	SessionExpectation struct {
		Required bool
		ID       string
	}
	// Interpretation contains only the semantic decision; storage owns payload identity.
	Interpretation struct {
		Verdict string
		Refusal string
		Session *SessionIdentity
	}
)

func ValidateSessionIdentity(v SessionIdentity) error {
	if config.ValidateProvider(v.Provider) != nil || !nonblank(v.ConversationID) {
		return ErrIdentityMismatch
	}
	return nil
}

func ValidateInterpretation(v Interpretation) error {
	if v.Session != nil {
		if err := ValidateSessionIdentity(*v.Session); err != nil {
			return err
		}
	}
	switch v.Verdict {
	case VerdictCommitted:
		if v.Refusal != "" {
			return errors.New("committed interpretation has refusal")
		}
	case VerdictRejected:
		if v.Refusal == "" {
			return errors.New("rejected interpretation has empty refusal")
		}
	default:
		return ErrInvalidEnum
	}
	return nil
}

// Stop facts carry only observed response values; taskdir derives the binding.
type (
	StopReplyFacts struct {
		NumericTaskID int64
		Action        string
		Acknowledged  bool
		Message       string
	}
	StopObservationFacts struct {
		NumericTaskID int64
		Terminated    bool
		State         string
	}
)
