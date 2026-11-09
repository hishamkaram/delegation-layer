package claude

import "github.com/hishamkaram/delegation-layer/internal/task"

// leadingStage is the boundary state of the scoped completion policy before
// the principal result. A resumed session can answer old background
// completions with empty zero-turn results that precede the caller's own
// result; the reducer accepts them only through this stage machine.
type leadingStage uint8

const (
	// leadingStart: no acknowledgement yet, and no root message since init.
	leadingStart leadingStage = iota
	// leadingAcknowledged: an acknowledgement was accepted since the last
	// boundary; one identical repeated init is permitted next.
	leadingAcknowledged
	// leadingReinitialized: that repeated init was consumed.
	leadingReinitialized
	// leadingRooted: a root message was seen since the last boundary, so no
	// acknowledgement or repeated init may follow.
	leadingRooted
)

// applyLeading decides one pre-principal record under the scoped policy and
// reports whether it fully handled the record. Every other record continues
// through the shared current-event rules. Frozen policies never route here.
func (a *completionAttribution) applyLeading(state *eventState, event claudeCurrentEvent) bool {
	if a == nil || !a.scoped || state.semanticErr != nil {
		return false
	}
	switch event.typeName {
	case "result":
		return a.routeLeadingResult(state, event)
	case "assistant", "user":
		if event.scope == claudeMessageScopeRoot {
			a.stage = leadingRooted
		}
	case "system":
		if state.initSeen && isClaudeInitEvent(event) {
			a.acceptLeadingInit(state, event)
			return true
		}
	}
	return false
}

// routeLeadingResult claims a result whose origin is a task notification; a
// result of any other origin is the caller's candidate principal.
func (a *completionAttribution) routeLeadingResult(state *eventState, event claudeCurrentEvent) bool {
	completion, err := decodeClaudeResultCompletion(event.value, a.advisoryWire())
	if err != nil || !completion.origin.present || completion.origin.kind != claudeOriginTaskNotification {
		return false
	}
	if bindClaudeCurrentSession(state, event) {
		a.acceptLeadingAcknowledgement(state, event, completion)
	}
	return true
}

func (a *completionAttribution) acceptLeadingAcknowledgement(state *eventState, event claudeCurrentEvent, completion claudeResultCompletion) {
	switch {
	case !state.initSeen:
		state.markSemantic("result precedes system/init")
	case a.stage == leadingRooted:
		state.markSemantic("leading acknowledgement follows a root message")
	case completion.origin.subkindPresent:
		state.markSemantic("leading acknowledgement has an origin subkind")
	case completion.index == nil || *completion.index != a.nextIndex:
		state.markSemantic("leading acknowledgement result_index is not consecutive from 0")
	default:
		a.recordLeadingAcknowledgement(state, event, completion)
	}
}

// recordLeadingAcknowledgement validates one acknowledgement's shape. A failed
// or refused one is retained through the follow-up refusal channel; a
// successful one must be exactly the documented empty zero-turn result. It
// never writes the principal result, answer, usage, or cost baseline.
func (a *completionAttribution) recordLeadingAcknowledgement(state *eventState, event claudeCurrentEvent, completion claudeResultCompletion) {
	quota := a.consumeQuota(completion)
	result, err := decodeClaudeCurrentResult(event.fields, state.init.sessionID, event.value, a.classifiesFailedRequests())
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	fault := a.statedFault(result, completion)
	fault.quota = quota
	fault.quotaAdvice = task.CloneQuotaAdvice(a.quotaConsumed)
	switch fault.refusal {
	case "":
		state.markSemantic("leading acknowledgement carries an answer")
		return
	case refusalEmpty:
		if !isEmptyZeroTurnResult(result, completion) {
			state.markSemantic("leading acknowledgement is not an empty zero-turn result")
			return
		}
	default:
		a.recordFollowUpFault(fault)
	}
	a.indexed = true
	a.nextIndex++
	a.stage = leadingAcknowledged
}

func isEmptyZeroTurnResult(result resultState, completion claudeResultCompletion) bool {
	return result.answerSeen && result.currentAnswerSet && result.currentAnswer.length == 0 && !result.currentAnswer.invalidNUL && completion.zeroTurns
}

// acceptLeadingInit admits one identical repeated init after an
// acknowledgement and before any root message.
func (a *completionAttribution) acceptLeadingInit(state *eventState, event claudeCurrentEvent) {
	if !bindClaudeCurrentSession(state, event) {
		return
	}
	if a.stage != leadingAcknowledged {
		state.markSemantic("system/init repeats outside a leading acknowledgement boundary")
		return
	}
	if a.requireSameInit(state, event.fields, "leading system/init changed the native identity") {
		a.stage = leadingReinitialized
	}
}
