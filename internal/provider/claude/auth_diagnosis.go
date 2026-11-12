package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	authenticationPredicateVersion               = "native-permissions-v7"
	authenticationWorkspaceWritePredicateVersion = "native-permissions-v8"
)

const authenticationPolicy = "; structured authentication diagnosis: only a root assistant event after init with the exact session_id and an unambiguous top-level error string authentication_failed may correlate with the next root result whose terminal_reason is api_error; every root result consumes that correlation; only the reported failure of an otherwise valid identity-matched stream reports provider_authentication_failed, with no provider text; nested messages, arbitrary content, stderr, unknown or ambiguous error forms never imply authentication failure"

func authenticationNativeContract(mode string) string {
	contract := followUpNativeContract(mode)
	if contract == "" {
		return ""
	}
	old, next := predicateVersion, authenticationPredicateVersion
	if mode == WorkspaceWriteMode {
		old, next = workspaceWritePredicateVersion, authenticationWorkspaceWritePredicateVersion
	}
	contract = strings.Replace(contract, `"version":"`+old+`"`, `"version":"`+next+`"`, 1)
	return strings.Replace(contract, followUpSelectionPolicy, followUpSelectionPolicy+authenticationPolicy, 1)
}

func authenticationInteractionContract() string {
	contract := strings.Replace(interactionContract(), "host clarification v7:", "host clarification v8:", 1)
	return strings.Replace(contract, followUpSelectionPolicy, followUpSelectionPolicy+authenticationPolicy, 1)
}

// FollowUpNativeContractForMode preserves the pre-authentication contract for
// consumers whose independently versioned predicates embed these exact bytes.
func FollowUpNativeContractForMode(mode string) string { return followUpNativeContract(mode) }

// Retained revisions preserve their exact contracts and completion behavior.
func followUpReference(mode string) task.PredicateRef {
	version := predicateVersion
	if mode == WorkspaceWriteMode {
		version = workspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(followUpNativeContract(mode)))}
}

func newFollowUpInterpreter(mode string) predicate.Interpreter {
	ref := followUpReference(mode)
	return interpreter{mode: mode, streamReference: &ref, completion: advisoryFollowUpCompletion}
}

func followUpInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v7", SHA256: task.ComputeSHA256([]byte(interactionContract() + mode + "\n"))}
}

func newFollowUpInteractionInterpreter(mode string) predicate.Interpreter {
	ref := followUpInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &ref, completion: advisoryFollowUpCompletion}
}

// The immutable reducer owns this correlation. It stores booleans only; raw
// authentication text is never retained or inspected.
func (a *completionAttribution) observeAuthentication(state *eventState, event claudeCurrentEvent) {
	if a == nil || !a.authentication || event.typeName != "assistant" || event.scope != claudeMessageScopeRoot {
		return
	}
	a.authPending = false
	if !state.initSeen || !claudeEventSessionIs(event, state.init.sessionID) {
		return
	}
	fields, err := event.value.Object("error")
	if err != nil {
		return
	}
	code, present, err := claudeCurrentOptionalString(fields, "error", 64)
	a.authPending = err == nil && present && code == "authentication_failed"
}

func (a *completionAttribution) consumeAuthentication(completion claudeResultCompletion) {
	a.authConsumed = a.authentication && a.authPending && completion.terminalReason == claudeTerminalAPIError
	a.authPending = false
}

func claudeAuthenticationFailed(input predicate.Input, state eventState, refusal, providerID string) bool {
	if providerID != Provider || !strings.HasPrefix(refusal, refusalProviderFailed) || input.Seal.InvocationState == task.InvocationStartFailed || input.Seal.Error != "" || state.semanticFault() || !state.initSeen || !identityMatchesProvider(input, state.init.sessionID, providerID) || state.attribution == nil || !state.attribution.authentication {
		return false
	}
	a := state.attribution
	if state.resultSeen && a.resultFault(state.result).refusal != "" {
		return a.principalAuth
	}
	return a.completionFault().authentication
}
