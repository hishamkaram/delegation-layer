package claude

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	interactiveRunning = `{"type":"system","subtype":"session_state_changed","state":"running"}`
	interactiveIdle    = `{"type":"system","subtype":"session_state_changed","state":"idle"}`
	interactiveAgent   = `{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"agent-1"}`
	interactiveSettled = `{"type":"system","subtype":"task_notification","task_id":"agent-1"}`
)

func interactiveInitLine(mode string) string {
	if mode == WorkspaceWriteMode {
		return writeInitLine(claudeTestUUID)
	}
	return initLine(claudeTestUUID)
}

func interactiveAssistant(text string) string {
	return fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":%q}]}}`, claudeTestUUID, text)
}

// interactiveFollowUpTrace keeps the input channel open for a tracked agent,
// then observes the provider-native notification turn that answers it.
func interactiveFollowUpTrace(mode string) []string {
	return []string{
		interactiveInitLine(mode), interactiveRunning, interactiveAgent,
		attributedResultLine(tracePrincipalText, `,"result_index":0`, 4, "0.1"), interactiveIdle, interactiveSettled,
		interactiveRunning, interactiveInitLine(mode), interactiveAssistant("neutral"),
		attributedResultLine("neutral", notificationAttribution(1), 1, "0.2"), interactiveIdle,
	}
}

func evaluateInteractiveTrace(t *testing.T, v predicate.Interpreter, lines []string) (task.Interpretation, string) {
	t.Helper()
	stdout := []byte(strings.Join(lines, "\n") + "\n")
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	var answer bytes.Buffer
	interpretation, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}

func TestInteractiveV4SelectsPrincipalAcrossLaterNotifications(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		// The frozen principal-only interactive-v6 keeps selecting the principal;
		// the current interactive-v7 frames every accepted root answer.
		frozenAdvisory, answer := evaluateInteractiveTrace(t, newAdvisoryInteractionInterpreter(mode), interactiveFollowUpTrace(mode))
		if frozenAdvisory.Session == nil || *frozenAdvisory.Session != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) || frozenAdvisory.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
			t.Fatalf("%s interactive-v6=%+v answer=%q", mode, frozenAdvisory, answer)
		}
		current, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), interactiveFollowUpTrace(mode))
		if current.Session == nil || *current.Session != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) {
			t.Fatalf("%s interactive-v7 session=%+v", mode, current.Session)
		}
		requireCommittedResponses(t, current, answer, interactiveFollowUpTrace(mode))
		// Frozen interactive-v3 rejects assistant output in a follow-up turn and
		// otherwise publishes the latest root result.
		if frozen, _ := evaluateInteractiveTrace(t, newPreAttributionInteractionInterpreter(mode), interactiveFollowUpTrace(mode)); frozen.Verdict != task.VerdictRejected {
			t.Fatalf("%s interactive-v3 replay changed: %+v", mode, frozen)
		}
		withoutMessages := append(interactiveFollowUpTrace(mode)[:8], interactiveFollowUpTrace(mode)[9:]...)
		frozen, frozenAnswer := evaluateInteractiveTrace(t, newPreAttributionInteractionInterpreter(mode), withoutMessages)
		if frozen.Verdict != task.VerdictCommitted || frozenAnswer != "neutral" {
			t.Fatalf("%s interactive-v3 replay changed: %+v answer=%q", mode, frozen, frozenAnswer)
		}
		closed := []string{interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), interactiveIdle, interactiveInitLine(mode), interactiveAssistant("neutral"), attributedResultLine("neutral", notificationAttribution(1), 1, "0.2")}
		// A follow-up without messages, and one after input closure, stay
		// evidence: the frozen interactive-v6 publishes the principal alone and
		// the current interactive-v7 frames both answers.
		for _, lines := range [][]string{withoutMessages, closed} {
			if frozenAdvisory, frozenAnswer := evaluateInteractiveTrace(t, newAdvisoryInteractionInterpreter(mode), lines); frozenAdvisory.Verdict != task.VerdictCommitted || frozenAnswer != tracePrincipalText {
				t.Fatalf("%s interactive-v6 selected a notification answer: %+v answer=%q", mode, frozenAdvisory, frozenAnswer)
			}
			current, answer = evaluateInteractiveTrace(t, newInteractionInterpreter(mode), lines)
			requireCommittedResponses(t, current, answer, lines)
		}
	}
}

func TestInteractiveV4RejectsUnattributedOrUnsettledCompletion(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		tests := map[string][]string{
			"notification only": {interactiveInitLine(mode), attributedResultLine("synthetic", notificationAttribution(0), 1, "0.1"), interactiveIdle},
			"later root result": {interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), attributedResultLine("x", `,"result_index":1`, 1, "0.2"), interactiveIdle},
			"aborted principal": {interactiveInitLine(mode), strings.Replace(attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), `"completed"`, `"aborted_tools"`, 1), interactiveIdle},
			"pending batch":     {interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), attributedResultLine("", notificationAttribution(1), 0, "0.2"), interactiveIdle},
			"failed follow-up":  append(interactiveFollowUpTrace(mode)[:9], `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"`+claudeTestUUID+`","errors":["failed"]`+notificationAttribution(1)+`}`, interactiveIdle),
		}
		for name, lines := range tests {
			interpretation, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), lines)
			if interpretation.Verdict != task.VerdictRejected || answer != "" {
				t.Fatalf("%s/%s interpretation=%+v answer=%q", mode, name, interpretation, answer)
			}
		}
	}
}

func liveAttributedInteraction(t *testing.T) *claudeInteraction {
	t.Helper()
	return liveAttributedInteractionIn(t, Mode)
}

func liveAttributedInteractionIn(t *testing.T, mode string) *claudeInteraction {
	t.Helper()
	request := task.TaskRecord{RootID: claudeTestRootID, TaskID: claudeTestTaskID, Mode: mode}
	protocol, err := newClaudeInteractionWithCapture(request, task.SessionExpectation{Required: true, ID: claudeTestUUID}, nil, true, scopedAttributedCompletion)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong protocol")
	}
	return p
}

func receiveClaudeRecord(t *testing.T, p *claudeInteraction, line string) execution.InteractionUpdate {
	t.Helper()
	update, err := p.ReceiveRecord(providerjson.Value{Reader: strings.NewReader(line), End: int64(len(line))})
	if err != nil {
		t.Fatalf("receive %s: %v", line, err)
	}
	return update
}

func TestLiveInteractiveV4ClosesOnlyAfterSettledFollowUps(t *testing.T) {
	p := liveAttributedInteraction(t)
	trace := interactiveFollowUpTrace(Mode)
	for index, line := range trace {
		update := receiveClaudeRecord(t, p, line)
		if update.Done != (index == len(trace)-1) {
			t.Fatalf("record %d closed=%v", index, update.Done)
		}
	}
	// A valid repeated initialization after closure is native follow-up
	// evidence, not a protocol fault. The stream is incomplete until that cycle
	// has its attributed answer and the session is idle again.
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	if !errors.Is(p.Complete(), task.ErrEvidenceFault) {
		t.Fatal("end of stream with an unanswered follow-up cycle completed")
	}
	receiveClaudeRecord(t, p, interactiveAssistant("neutral"))
	receiveClaudeRecord(t, p, attributedResultLine("neutral", notificationAttribution(2), 1, "0.3"))
	receiveClaudeRecord(t, p, interactiveIdle)
	if p.Complete() != nil {
		t.Fatal("settled post-closure follow-up cycle did not complete")
	}
	batch := liveAttributedInteraction(t)
	for _, line := range []string{initLine(claudeTestUUID), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1"), attributedResultLine("", notificationAttribution(1), 0, "0.1")} {
		receiveClaudeRecord(t, batch, line)
	}
	if update := receiveClaudeRecord(t, batch, interactiveIdle); update.Done {
		t.Fatal("pending batch acknowledgement closed input")
	}
	receiveClaudeRecord(t, batch, attributedResultLine("neutral", notificationAttribution(2), 1, "0.2"))
	if update := receiveClaudeRecord(t, batch, interactiveIdle); !update.Done || batch.Complete() != nil {
		t.Fatalf("answered batch did not settle: %+v", update)
	}
	synthetic := liveAttributedInteraction(t)
	receiveClaudeRecord(t, synthetic, initLine(claudeTestUUID))
	notification := attributedResultLine("x", notificationAttribution(0), 1, "0.1")
	if _, err := synthetic.ReceiveRecord(providerjson.Value{Reader: strings.NewReader(notification), End: int64(len(notification))}); err == nil {
		t.Fatal("notification-only principal was accepted live")
	}
	if !errors.Is(synthetic.Complete(), task.ErrEvidenceFault) {
		t.Fatal("rejected live completion reported success")
	}
}
