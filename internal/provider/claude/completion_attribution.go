package claude

import (
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// completionPolicy selects the versioned interpretation of Claude root
// results. Its zero value is the frozen policy bound to every current-wire
// reference sealed before completion attribution existed.
type completionPolicy uint8

const (
	// singleResultCompletion keeps the frozen pre-attribution behavior: one
	// result for print evidence and the latest root result for interactive-v3.
	singleResultCompletion completionPolicy = iota
	// unscopedAttributedCompletion is the attributed policy as first admitted:
	// every assistant or user message after the principal result opens a root
	// follow-up cycle, whatever its parent_tool_use_id. It is frozen for the
	// retained references sealed under it and is never a fresh default.
	unscopedAttributedCompletion
	// scopedAttributedCompletion selects the caller's principal result and
	// admits later provider-native background notification cycles in the same
	// invocation only when each one is attributed and settled. Only root
	// messages take part in cycles; nested agent messages are validated and
	// otherwise excluded, and an invalid message scope is malformed. A resumed
	// session's empty zero-turn leading acknowledgements may precede the
	// caller's result without owning it.
	scopedAttributedCompletion
	// advisoryScopedCompletion is the current policy: the scoped reducer over
	// the advisory wire projection of current_wire.go. Only session identity
	// and the requested native permission are init authority, opaque payloads
	// are not checked for unique keys, unknown content blocks are opaque, and
	// an unknown completion reason is an unsupported completion.
	advisoryScopedCompletion
	// advisoryFollowUpCompletion exposes every accepted root answer.
	advisoryFollowUpCompletion
	// projectionFollowUpCompletion is the frozen follow-up policy of the
	// projection revisions: it exposes every accepted root answer but, sealed
	// before failed-request classification, keeps a failed final request
	// malformed and never correlates a quota.
	projectionFollowUpCompletion
	// authenticationFollowUpCompletion adds structured root authentication
	// diagnosis while retaining the follow-up verdict and answer policy.
	authenticationFollowUpCompletion
)

// completionAttribution is the canonical current completion reducer for one
// Claude invocation. It owns principal selection, the result sequence and its
// counters, pending follow-up cycles and coalesced batches, typed completion
// metadata, and the resulting completion faults. eventState.result holds the
// principal result and its answer spans; the reducer never replaces it. The
// ordinary, CCR, and interactive parsers write it from one record stream;
// predicates and the interactive closure observer read it.
//
// Invariant: results carry consecutive result_index values from 0 once any
// counter is seen. After the principal result, every later result has a
// task-notification origin without subkind; a follow-up cycle ends only with a
// non-empty attributed result, and no cycle or batch acknowledgement remains
// pending at EOF. Under the scoped policy only root messages open or advance a
// cycle, and the principal result may be preceded by leading acknowledgements
// that never own the answer, usage, or closure.
type completionAttribution struct {
	// scoped is fixed by the immutable completion policy: nested agent
	// messages are excluded from cycles, invalid message scopes reject, and
	// leading acknowledgements may precede the principal result.
	scoped bool
	// project selects one callback-scoped answer at a time, never a history.
	project            bool
	selected           *claudeSelectedAnswer
	hasFollowUpAnswers bool
	// Authentication state is private to this immutable policy's reducer.
	// Each root result consumes the pending same-session assistant error.
	authentication bool
	authPending    bool
	authConsumed   bool
	principalAuth  bool
	// failedRequests classifies the documented failed final request, subtype
	// success with is_error true, as a provider failure instead of malformed
	// output, and correlates its api_error with the latest same-session
	// rate-limit rejection; quota_attribution.go owns the correlation.
	failedRequests bool
	quotaRejected  bool
	quotaPending   *task.QuotaAdvice
	quotaConsumed  *task.QuotaAdvice
	principalQuota bool
	// advisory is fixed by the immutable completion policy and selects the
	// advisory wire projection, init authority, and unsupported reasons.
	advisory bool
	// stage is the pre-principal boundary state of the scoped policy; see
	// completion_leading.go. The principal result resets it.
	stage         leadingStage
	indexed       bool
	nextIndex     int64
	followUps     int
	cycleOpen     bool
	cycleInit     bool
	cycleMessages bool
	batchPending  bool
	// principalFault is stated by the principal's typed metadata; fault is
	// the first failed follow-up or leading acknowledgement. Neither replaces
	// a principal result refusal, which the predicate evaluates first.
	principalFault       completionFault
	fault                completionFault
	settledCostUSD       *string
	settledUsagePresent  bool
	settledUsageReliable bool
}

func newCompletionAttribution(policy completionPolicy) *completionAttribution {
	switch policy {
	case unscopedAttributedCompletion:
		return &completionAttribution{}
	case scopedAttributedCompletion:
		return &completionAttribution{scoped: true}
	case advisoryScopedCompletion:
		return &completionAttribution{scoped: true, advisory: true}
	case authenticationFollowUpCompletion:
		return &completionAttribution{scoped: true, advisory: true, project: true, failedRequests: true, authentication: true}
	case advisoryFollowUpCompletion:
		return &completionAttribution{scoped: true, advisory: true, project: true, failedRequests: true}
	case projectionFollowUpCompletion:
		return &completionAttribution{scoped: true, advisory: true, project: true}
	case singleResultCompletion:
		return nil
	}
	return nil
}

// applyClaudeCompletionEvent routes one decoded record through the selected
// completion policy. Before the principal result, and for the single-result
// policy throughout, the shared current-event rules apply unchanged.
func applyClaudeCompletionEvent(state *eventState, event claudeCurrentEvent) {
	attribution := state.attribution
	attribution.observeAuthentication(state, event)
	if attribution.rejectsMessageScope(event) {
		state.markSemantic(event.typeName + " parent_tool_use_id is neither null nor a nonblank string")
		return
	}
	if attribution == nil || !state.resultSeen {
		if attribution.applyLeading(state, event) {
			return
		}
		applyClaudeCurrentEvent(state, event)
		if attribution != nil && event.typeName == "result" && state.resultSeen && state.semanticErr == nil {
			attribution.acceptPrincipal(state, event.value)
		}
		return
	}
	if !isClaudeFollowUpCycleEvent(event) {
		applyClaudeCurrentEvent(state, event)
		return
	}
	if state.semanticErr != nil || !bindClaudeCurrentSession(state, event) {
		return
	}
	switch event.typeName {
	case "result":
		attribution.acceptFollowUp(state, event)
	case "system":
		attribution.acceptFollowUpInit(state, event.fields)
	default:
		attribution.acceptFollowUpMessage(state, event)
	}
}

// rejectsMessageScope reports an invalid message scope under the scoped
// policy, before or after the principal result. Frozen policies never do.
func (a *completionAttribution) rejectsMessageScope(event claudeCurrentEvent) bool {
	return a != nil && a.scoped && event.scope == claudeMessageScopeInvalid
}

// excludesNestedMessage reports whether event is a nested agent message that
// the scoped policy keeps out of root cycles and root tool-result resolution.
func (a *completionAttribution) excludesNestedMessage(event claudeCurrentEvent) bool {
	return a != nil && a.scoped && event.scope == claudeMessageScopeNested
}

// requiresIdleEndOfStream reports whether end of stream must re-prove the
// current settlement after a sticky input closure. Only the scoped policy does;
// the frozen policies keep the end-of-stream rule they were admitted under.
func (a *completionAttribution) requiresIdleEndOfStream() bool {
	return a != nil && a.scoped
}

func isClaudeFollowUpCycleEvent(event claudeCurrentEvent) bool {
	switch event.typeName {
	case "result", "assistant", "user":
		return true
	case "system":
		return isClaudeInitEvent(event)
	default:
		return false
	}
}

func isClaudeInitEvent(event claudeCurrentEvent) bool {
	subtype, present, err := claudeCurrentOptionalString(event.fields, "subtype", 256)
	return event.typeName == "system" && err == nil && present && subtype == "init"
}

func (a *completionAttribution) acceptPrincipal(state *eventState, value providerjson.Value) {
	completion, err := decodeClaudeResultCompletion(value, a.advisoryWire())
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if completion.origin.present && completion.origin.kind != claudeOriginHuman {
		state.markSemantic("first result is not attributed to the caller turn")
		return
	}
	if a.indexed {
		// Leading acknowledgements consumed the counters from 0; the caller's
		// result must follow them without a gap.
		if completion.index == nil || *completion.index != a.nextIndex {
			state.markSemantic("principal result_index does not follow the leading acknowledgements")
			return
		}
		a.nextIndex++
	} else if completion.index != nil {
		if *completion.index != 0 {
			state.markSemantic("first result_index is not 0")
			return
		}
		a.indexed = true
		a.nextIndex = 1
	}
	a.stage = leadingStart
	a.consumeAuthentication(completion)
	a.principalQuota = a.consumeQuota(completion)
	a.principalAuth = a.authConsumed
	a.principalFault = completion.fault()
	a.principalFault.authentication = a.principalAuth
	a.principalFault.quota = a.principalQuota
	a.principalFault.quotaAdvice = task.CloneQuotaAdvice(a.quotaConsumed)
	if a.project {
		a.selected = &claudeSelectedAnswer{index: a.nextIndex - 1, origin: "caller", text: state.result.currentAnswer}
	}
}

func (a *completionAttribution) acceptFollowUp(state *eventState, event claudeCurrentEvent) {
	completion, err := decodeClaudeResultCompletion(event.value, a.advisoryWire())
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	a.consumeAuthentication(completion)
	quota := a.consumeQuota(completion)
	if reason := followUpSequenceFault(a, completion); reason != "" {
		state.markSemantic(reason)
		return
	}
	result, err := decodeClaudeCurrentResult(event.fields, state.init.sessionID, event.value, a.classifiesFailedRequests())
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	a.nextIndex++
	a.followUps++
	a.settledCostUSD = result.totalCostUSD
	a.settledUsagePresent = result.currentUsagePresent
	a.settledUsageReliable = result.currentUsageReliable
	fault := a.statedFault(result, completion)
	if fault.refusal == refusalEmpty && completion.zeroTurns {
		// A coalesced batch acknowledges each earlier completion with an
		// empty zero-turn result; the last result carries the answering turn.
		a.batchPending = true
		return
	}
	a.cycleOpen, a.cycleInit, a.cycleMessages, a.batchPending = false, false, false, false
	if fault.refusal == refusalEmpty {
		fault = completionFault{refusal: refusalIncomplete}
	}
	if fault.refusal != "" {
		fault.authentication = a.authConsumed
		fault.quota = quota
		fault.quotaAdvice = task.CloneQuotaAdvice(a.quotaConsumed)
		a.recordFollowUpFault(fault)
	} else if a.project {
		a.hasFollowUpAnswers = true
		a.selected = &claudeSelectedAnswer{index: a.nextIndex - 1, origin: "task-notification", text: result.currentAnswer}
	}
}

func followUpSequenceFault(a *completionAttribution, completion claudeResultCompletion) string {
	if !a.indexed || completion.index == nil {
		return "multiple result events require consistent result_index counters"
	}
	if *completion.index != a.nextIndex {
		return "result_index is not consecutive"
	}
	if completion.origin.kind != claudeOriginTaskNotification || completion.origin.subkindPresent {
		return "later result is not an attributed background notification"
	}
	return ""
}

func (a *completionAttribution) recordFollowUpFault(fault completionFault) {
	if a.fault.refusal == "" {
		a.fault = fault
	}
}

func (a *completionAttribution) acceptFollowUpInit(state *eventState, fields map[string]providerjson.Value) {
	if a.cycleInit || a.cycleMessages {
		state.markSemantic("system/init repeats within a follow-up cycle")
		return
	}
	if a.requireSameInit(state, fields, "follow-up system/init changed the native identity") {
		a.cycleOpen, a.cycleInit = true, true
	}
}

// requireSameInit is the one decode-and-compare step for a repeated
// system/init, shared by the leading and follow-up boundaries. Each caller
// owns its own stage guard and supplies the mismatch fault text; a false
// result means the state was already marked malformed.
func (a *completionAttribution) requireSameInit(state *eventState, fields map[string]providerjson.Value, mismatch string) bool {
	init, err := a.decodeInit(fields, state.mode)
	if err != nil {
		state.markSemantic(err.Error())
		return false
	}
	if !a.sameInit(init, state.init) {
		state.markSemantic(mismatch)
		return false
	}
	return true
}

func (a *completionAttribution) acceptFollowUpMessage(state *eventState, event claudeCurrentEvent) {
	if err := validateClaudeCurrentMessageEvent(event.fields, event.typeName, a.advisoryWire()); err != nil {
		state.markSemantic(err.Error())
		return
	}
	if a.excludesNestedMessage(event) {
		return
	}
	a.cycleOpen, a.cycleMessages = true, true
}

// settled reports whether no follow-up cycle or batch acknowledgement awaits
// its answering result. The interactive closure observer reads it.
func (a *completionAttribution) settled() bool {
	return a == nil || (!a.cycleOpen && !a.batchPending)
}

// finish runs once after EOF. Follow-up accounting replaces, never adds to,
// the principal's earlier cumulative whole-tree cost; main-agent usage stays
// bound to the principal turn.
func (a *completionAttribution) finish(state *eventState) {
	if a == nil || state.semanticErr != nil || !state.resultSeen {
		return
	}
	if a.cycleOpen {
		state.markSemantic("a follow-up cycle has no attributed result before EOF")
		return
	}
	if a.batchPending {
		a.recordFollowUpFault(completionFault{refusal: refusalIncomplete})
	}
	if a.followUps == 0 {
		return
	}
	state.result.totalCostUSD = a.settledCostUSD
	state.result.currentUsagePresent = state.result.currentUsagePresent || a.settledUsagePresent
	state.result.currentUsageReliable = state.result.currentUsageReliable && a.settledUsageReliable
}

// completionRefusal is the reducer's verdict after the principal result's own
// refusal: first the principal's typed metadata, then any follow-up failure.
func (a *completionAttribution) completionRefusal() string {
	return a.completionFault().refusal
}

func (a *completionAttribution) completionFault() completionFault {
	if a == nil {
		return completionFault{}
	}
	if a.principalFault.refusal != "" {
		return a.principalFault
	}
	return a.fault
}
