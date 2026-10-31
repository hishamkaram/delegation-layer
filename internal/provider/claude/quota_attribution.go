package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// claudeRateLimitRejected is the only rate_limit_info status that the
	// follow-up policy correlates with a root api_error result. Warnings and
	// every other or unknown status never state an exhausted quota.
	claudeRateLimitRejected       = "rejected"
	claudeTerminalAPIError        = "api_error"
	claudeRateLimitStatusMaxBytes = 64
)

// classifiesFailedRequests reports whether the immutable policy classifies a
// documented failed final request as a provider failure.
func (a *completionAttribution) classifiesFailedRequests() bool {
	return a != nil && a.failedRequests
}

// observeRateLimit records whether the latest same-session rate-limit event
// states a structured rejection. Only the follow-up policy reads it. The
// rate_limit_info object is projected separately from the event wire, so an
// absent, null, mistyped, ambiguous, or unknown value clears the pending
// rejection and never changes the stream verdict. No provider value is kept.
func (a *completionAttribution) observeRateLimit(state *eventState, event claudeCurrentEvent) {
	if a == nil || !a.failedRequests {
		return
	}
	a.quotaRejected = state.initSeen && claudeEventSessionIs(event, state.init.sessionID) && claudeRateLimitRejection(event.value)
}

// consumeQuota attributes the pending rejection to one root result and resets
// it, so a rejection settled by an earlier result never describes a later one.
func (a *completionAttribution) consumeQuota(completion claudeResultCompletion) bool {
	if a == nil || !a.failedRequests {
		return false
	}
	correlated := a.quotaRejected && completion.terminalReason == claudeTerminalAPIError
	a.quotaRejected = false
	return correlated
}

// quotaFault reports whether the result whose fault the predicate reports was
// correlated with an exhausted quota, using the precedence of
// refusalForProvider and unsupportedCompletionField.
func (a *completionAttribution) quotaFault(state eventState) bool {
	if a == nil || !a.failedRequests {
		return false
	}
	if state.resultSeen {
		if fault := a.resultFault(state.result); fault.refusal != "" {
			return a.principalQuota
		}
	}
	return a.completionFault().quota
}

func claudeEventSessionIs(event claudeCurrentEvent, sessionID string) bool {
	value, ok := event.fields["session_id"]
	if !ok {
		return false
	}
	observed, err := value.String(128)
	return err == nil && observed == sessionID
}

func claudeRateLimitRejection(value providerjson.Value) bool {
	fields, err := value.Object("rate_limit_info")
	if err != nil {
		return false
	}
	info, ok := fields["rate_limit_info"]
	if !ok || claudeCurrentValueIsNull(info) {
		return false
	}
	infoFields, err := info.Object("status")
	if err != nil {
		return false
	}
	status, present, err := claudeCurrentOptionalString(infoFields, "status", claudeRateLimitStatusMaxBytes)
	return err == nil && present && status == claudeRateLimitRejected
}

// claudeQuotaExhausted reports whether a provider-failed refusal is the root
// api_error result that the follow-up policy correlated with the latest
// same-session structured rate-limit rejection. A start or capture fault, a
// malformed stream, or an identity mismatch never reports a quota.
func claudeQuotaExhausted(input predicate.Input, state eventState, refusal, providerID string) bool {
	if !strings.HasPrefix(refusal, refusalProviderFailed) || input.Seal.InvocationState == task.InvocationStartFailed || input.Seal.Error != "" ||
		state.semanticFault() || !state.initSeen || !identityMatchesProvider(input, state.init.sessionID, providerID) {
		return false
	}
	return state.attribution.quotaFault(state)
}

// claudeRejectionDiagnostic is the sanitized diagnostic of a rejected current
// stream: the constant quota diagnostic when the follow-up policy correlated
// the reported failure with an exhausted quota, and the refusal's otherwise.
func claudeRejectionDiagnostic(input predicate.Input, state eventState, refusal, providerID string) task.OutputDiagnostic {
	if claudeQuotaExhausted(input, state, refusal, providerID) {
		return claudeQuotaDiagnostic()
	}
	return claudeRefusalDiagnostic(refusal, state)
}

// claudeQuotaDiagnostic is constant: it never carries a reset time, window,
// overage reason, or provider error text.
func claudeQuotaDiagnostic() task.OutputDiagnostic {
	return task.OutputDiagnostic{
		Code:    "provider_quota_exhausted",
		Message: "Claude reported that its usage quota was exhausted.",
		Field:   "rate_limit_info",
	}
}
