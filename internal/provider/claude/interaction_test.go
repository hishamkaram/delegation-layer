package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func initializedClaudeInteraction(t *testing.T) *claudeInteraction {
	t.Helper()
	req := task.TaskRecord{RootID: claudeTestRootID, TaskID: claudeTestTaskID, Mode: Mode}
	protocol, err := newClaudeInteraction(req, task.SessionExpectation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong protocol")
	}
	if _, err = p.Start([]byte("ask one question")); err != nil {
		t.Fatal(err)
	}
	receiveClaudeInteraction(t, p, `{"type":"control_response","response":{"request_id":"delegate-initialize","subtype":"success","response":{}}}`)
	update := receiveClaudeInteraction(t, p, initLine(p.expected))
	if len(update.Events) != 1 || update.Events[0].Kind != "ready" {
		t.Fatalf("no ready event: %+v", update)
	}
	return p
}

func receiveClaudeInteraction(t *testing.T, p *claudeInteraction, line string) execution.InteractionUpdate {
	t.Helper()
	update, err := p.Receive([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func TestClaudeInteractionAnswersOnlyQuestions(t *testing.T) {
	p := initializedClaudeInteraction(t)
	update := receiveClaudeInteraction(t, p, `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"tool-1","input":{"questions":[{"question":"Choose a color","header":"Color","multiSelect":true,"options":[{"label":"Blue"},{"label":"Red"}]}]}}}`)
	question := update.Events[0].Request
	if !question.Blocking || !question.Questions[0].Multiple || !question.Questions[0].FreeText {
		t.Fatalf("question lost semantics: %+v", question)
	}
	id := strings.Repeat("a", 32)
	frame, err := p.Encode(task.InteractionMessage{ID: id, Kind: "reply", TurnID: p.turnID, RequestID: question.ID, Answers: map[string][]string{"1": {"Blue", "Red"}}})
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string `json:"behavior"`
				UpdatedInput struct {
					Answers map[string]string `json:"answers"`
				} `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err = json.Unmarshal(frame, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Response.RequestID != "native-question" || reply.Response.Response.Behavior != "allow" || reply.Response.Response.UpdatedInput.Answers["Choose a color"] != "Blue, Red" {
		t.Fatalf("incorrect reply: %s", frame)
	}
	update = receiveClaudeInteraction(t, p, fmt.Sprintf(`{"type":"user","session_id":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"answered"}]}}`, p.expected))
	if len(update.Events) != 2 || update.Events[0].Delivery.State != "acknowledged" || update.Events[1].Kind != "resolved" {
		t.Fatalf("native resolution not observed: %+v", update)
	}
	if _, err = p.Encode(task.InteractionMessage{ID: id, Kind: "steer", TurnID: p.turnID, Text: "guidance"}); err == nil {
		t.Fatal("unsupported steering accepted")
	}
}

func TestClaudeInteractionDoesNotPromoteToolApprovalToQuestion(t *testing.T) {
	p := initializedClaudeInteraction(t)
	update := receiveClaudeInteraction(t, p, `{"type":"control_request","request_id":"permission-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"touch file"}}}`)
	if len(update.Events) != 0 || len(update.Outbound) != 1 || !bytes.Contains(update.Outbound[0], []byte(`"behavior":"deny"`)) {
		t.Fatalf("tool permission was granted or exposed as clarification: %+v", update)
	}
}

func TestClaudeInteractionKeepsChannelOpenForBackgroundAgent(t *testing.T) {
	p := initializedClaudeInteraction(t)
	receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"running"}`)
	receiveClaudeInteraction(t, p, `{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"agent-1"}`)
	update := receiveClaudeInteraction(t, p, resultLine(p.expected, "intermediate"))
	if update.Done {
		t.Fatal("intermediate result closed input")
	}
	update = receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	if update.Done {
		t.Fatal("idle hid running native agent")
	}
	update = receiveClaudeInteraction(t, p, `{"type":"system","subtype":"task_notification","task_id":"agent-1"}`)
	if update.Done {
		t.Fatal("background completion reused an earlier root result")
	}
	receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"running"}`)
	update = receiveClaudeInteraction(t, p, initLine(p.expected))
	if len(update.Events) != 0 {
		t.Fatal("background initialization published another ready event")
	}
	receiveClaudeInteraction(t, p, resultLine(p.expected, "final after background work"))
	update = receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	if !update.Done || p.Complete() != nil {
		t.Fatalf("final root result did not complete: %+v", update)
	}
	if _, err := p.Receive([]byte(resultLine(p.expected, "second final"))); err == nil {
		t.Fatal("second completion accepted")
	}
}

func TestClaudeInteractionCannotEraseEarlierProviderFailure(t *testing.T) {
	p := initializedClaudeInteraction(t)
	receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"running"}`)
	failed := errorResultLine(p.expected, "error_during_execution", "provider error")
	receiveClaudeInteraction(t, p, failed)
	receiveClaudeInteraction(t, p, resultLine(p.expected, "later success"))
	receiveClaudeInteraction(t, p, `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	if p.Complete() == nil {
		t.Fatal("later success erased an earlier native failure")
	}
}

func TestClaudeInteractionSealedResultRequiresSettledSuccessfulRoot(t *testing.T) {
	id, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	running := `{"type":"system","subtype":"session_state_changed","state":"running"}`
	idle := `{"type":"system","subtype":"session_state_changed","state":"idle"}`
	start := `{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"agent-1"}`
	settled := `{"type":"system","subtype":"task_notification","task_id":"agent-1"}`
	for _, test := range []struct {
		name    string
		events  []string
		verdict string
	}{
		{"successful root", []string{running, resultLine(id, "final"), idle}, task.VerdictCommitted},
		{"background wakeup still pending", []string{running, start, resultLine(id, "intermediate"), idle, settled}, task.VerdictRejected},
		{"final after background", []string{running, start, resultLine(id, "intermediate"), idle, settled, running, resultLine(id, "final"), idle}, task.VerdictCommitted},
		{"background reinitialization", []string{running, start, resultLine(id, "intermediate"), settled, initLine(id), resultLine(id, "final"), idle}, task.VerdictCommitted},
		{"changed initialization identity", []string{running, initLine("00000000-0000-0000-0000-000000000001"), resultLine(id, "final"), idle}, task.VerdictRejected},
		{"changed initialization permission", []string{running, strings.Replace(initLine(id), `"plan"`, `"bypassPermissions"`, 1), resultLine(id, "final"), idle}, task.VerdictRejected},
		{"prior error retained", []string{running, errorResultLine(id, "error_during_execution", "failed"), resultLine(id, "final"), idle}, task.VerdictRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout := []byte(strings.Join(append([]string{initLine(id)}, test.events...), "\n") + "\n")
			interpreter := interactionInterpreter{mode: Mode}
			input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, interpreter.Reference())}
			var out bytes.Buffer
			result, evaluateErr := interpreter.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &out)
			if evaluateErr != nil || result.Verdict != test.verdict {
				t.Fatalf("result %+v err %v", result, evaluateErr)
			}
			if result.Verdict == task.VerdictCommitted && out.String() != "final" {
				t.Fatalf("wrong root answer: %q", out.String())
			}
			if result.Verdict == task.VerdictRejected && out.Len() != 0 {
				t.Fatal("rejected stream wrote answer")
			}
		})
	}
}

func TestCurrentClaudeInteractionStreamsLargeRecordAndWarnsOnUsage(t *testing.T) {
	largeOpaque := strings.Repeat("opaque", (task.MaxControlRecordSize/6)+4096)
	result := resultLine(claudeTestUUID, "interactive answer")
	result = strings.Replace(result, `,"session_id":`, fmt.Sprintf(`,"opaque":%q,"session_id":`, largeOpaque), 1)
	result = strings.Replace(result, `"input_tokens":12`, `"input_tokens":-1`, 1)
	stdout := []byte(strings.Join([]string{initLine(claudeTestUUID), result, `{"type":"system","subtype":"session_state_changed","state":"idle"}`}, "\n"))
	var answer bytes.Buffer
	interpreter := interactionInterpreter{mode: Mode}
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, interpreter.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	interpretation, err := interpreter.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != "interactive answer" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, answer.String(), err)
	}
	if len(interpretation.Diagnostics) != 1 || interpretation.Diagnostics[0].Code != "optional_usage_unavailable" || strings.Contains(interpretation.Diagnostics[0].Message, "opaque") {
		t.Fatalf("interaction diagnostics=%+v", interpretation.Diagnostics)
	}
}
