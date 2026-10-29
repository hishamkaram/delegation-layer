package provider

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NativeFactsRefusalReason is the fixed, nonsecret cause a provider gives for
// refusing a native inspection projection.
type NativeFactsRefusalReason uint8

const (
	// NativeFactsInvalid means a projection does not decode under the
	// provider's own projection contract.
	NativeFactsInvalid NativeFactsRefusalReason = iota + 1
	// NativeCapabilityChanged means both projections are valid, but the
	// recorded launch depends on the capability that changed.
	NativeCapabilityChanged
)

// NativeFactsRefusalError carries only its reason. It never holds native
// bytes, a provider error, or any other cause.
type NativeFactsRefusalError struct {
	Reason NativeFactsRefusalReason
}

func (e NativeFactsRefusalError) Error() string {
	switch e.Reason {
	case NativeFactsInvalid:
		return "provider native inspection facts are invalid"
	case NativeCapabilityChanged:
		return "provider native capability changed since admission"
	default:
		return "provider native facts refusal has an unknown reason"
	}
}

// NativeFactsRefusal classifies the whole error a candidate's
// AcceptRefreshedNativeFacts returns. It reports a reason only when every leaf
// cause is a refusal with that one known reason; a provider sentinel, another
// cause, or a mix of reasons is not a refusal.
func NativeFactsRefusal(err error) (NativeFactsRefusalReason, bool) {
	var refusal NativeFactsRefusalError
	if !errors.As(err, &refusal) || (refusal.Reason != NativeFactsInvalid && refusal.Reason != NativeCapabilityChanged) {
		return 0, false
	}
	return refusal.Reason, task.AllErrorCauses(err, func(cause error) bool {
		var leaf NativeFactsRefusalError
		return errors.As(cause, &leaf) && leaf.Reason == refusal.Reason
	})
}

// NativeCapabilityUnavailable reports whether a preparation refusal is wholly
// a capability that the recorded launch requires and the current provider no
// longer offers: unavailable-profile markers joined only with
// NativeCapabilityChanged refusals.
func NativeCapabilityUnavailable(err error) bool {
	var refusal NativeFactsRefusalError
	if !errors.Is(err, ErrProfileUnavailable) || !errors.As(err, &refusal) {
		return false
	}
	return task.AllErrorCauses(err, func(cause error) bool {
		var leaf NativeFactsRefusalError
		return errors.Is(cause, ErrProfileUnavailable) || (errors.As(cause, &leaf) && leaf.Reason == NativeCapabilityChanged)
	})
}
