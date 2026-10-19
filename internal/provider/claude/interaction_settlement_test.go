package claude

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const interactiveRequiresAction = `{"type":"system","subtype":"session_state_changed","state":"requires_action"}`

// closedInteractiveTrace is a validly closed interactive invocation: the
// principal result, then an idle session with no tracked agent or pending cycle.
func closedInteractiveTrace(mode string) []string {
	return []string{interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), interactiveIdle}
}

// reopenedFollowUp is a provider-native notification cycle after input closure:
// the same session wakes with a repeated init and answers with an attributed
// follow-up result.
func reopenedFollowUp(mode string) []string {
	return []string{interactiveInitLine(mode), interactiveAssistant("neutral"), attributedResultLine("neutral", notificationAttribution(1), 1, "0.2")}
}

type reopenedSettlementCase struct {
	name    string
	tail    []string
	settled bool
}

// reopenedSettlementCases states which evidence after input closure leaves the
// invocation settled under the scoped interactive-v5 revision. Only the current
// session state and tracked agents decide; the earlier closure does not. The
// frozen interactive-v4 revision never re-checks them at end of stream; see
// eofRevisionCases.
func reopenedSettlementCases(mode string) []reopenedSettlementCase {
	follow := reopenedFollowUp(mode)
	with := func(prefix []string, suffix ...string) []string {
		return append(append(append([]string{}, prefix...), follow...), suffix...)
	}
	return []reopenedSettlementCase{
		{name: "idle after follow-up", tail: with([]string{interactiveRunning}, interactiveIdle), settled: true},
		{name: "follow-up without final idle", tail: with([]string{interactiveRunning}), settled: false},
		{name: "action required after follow-up", tail: with([]string{interactiveRunning}, interactiveRequiresAction), settled: false},
		{name: "agent settled before follow-up", tail: with([]string{interactiveRunning, interactiveAgent, interactiveSettled}, interactiveIdle), settled: true},
		{name: "agent unsettled after follow-up", tail: with([]string{interactiveRunning, interactiveAgent}, interactiveIdle), settled: false},
	}
}

func forEachSettlementCase(t *testing.T, check func(t *testing.T, mode string, tc reopenedSettlementCase)) {
	t.Helper()
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, tc := range reopenedSettlementCases(mode) {
			t.Run(mode+"/"+tc.name, func(t *testing.T) { check(t, mode, tc) })
		}
	}
}

func TestInteractiveV5SealedPredicateRequiresCurrentSettlementAfterClosure(t *testing.T) {
	forEachSettlementCase(t, func(t *testing.T, mode string, tc reopenedSettlementCase) {
		lines := append(closedInteractiveTrace(mode), tc.tail...)
		interpretation, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), lines)
		if !tc.settled {
			if interpretation.Verdict != task.VerdictRejected || answer != "" {
				t.Fatalf("unsettled trace published: %+v answer=%q", interpretation, answer)
			}
			return
		}
		if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
			t.Fatalf("settled trace rejected or published a notification answer: %+v answer=%q", interpretation, answer)
		}
		if interpretation.Session == nil || *interpretation.Session != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) {
			t.Fatalf("session=%+v", interpretation.Session)
		}
	})
}

func TestLiveInteractiveV5CompleteRequiresCurrentSettlementAfterClosure(t *testing.T) {
	forEachSettlementCase(t, func(t *testing.T, mode string, tc reopenedSettlementCase) {
		p := liveAttributedInteractionIn(t, mode)
		var closure execution.InteractionUpdate
		for _, line := range closedInteractiveTrace(mode) {
			closure = receiveClaudeRecord(t, p, line)
		}
		if !closure.Done {
			t.Fatal("closed trace did not close input")
		}
		if err := p.Complete(); err != nil {
			t.Fatalf("closed trace did not complete: %v", err)
		}
		for _, line := range tc.tail {
			receiveClaudeRecord(t, p, line)
		}
		err := p.Complete()
		if tc.settled && err != nil {
			t.Fatalf("settled trace failed: %v", err)
		}
		if !tc.settled && !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("unsettled trace completed: %v", err)
		}
	})
}
