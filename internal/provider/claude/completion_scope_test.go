package claude

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Sanitized native-shaped fixtures for nested-agent messages. Claude Code
// stamps every complete nested assistant/user message with the opaque
// parent_tool_use_id of the agent tool call that spawned it; root messages
// carry null. The fixtures keep that shape and nothing else of a real trace.
const (
	scopeRootParent      = `,"parent_tool_use_id":null`
	scopeChildParent     = `,"parent_tool_use_id":"toolu_scope_agent"`
	scopeAgentUpdated    = `{"type":"system","subtype":"task_updated","task_id":"agent-1","patch":{"status":"completed"}}`
	scopeTextBlock       = `{"type":"text","text":"neutral"}`
	scopeAgentToolUse    = `{"type":"tool_use","id":"toolu_scope_agent","name":"Agent","input":{"prompt":"neutral"}}`
	scopeChildToolUse    = `{"type":"tool_use","id":"toolu_scope_child","name":"Bash","input":{"command":"true"}}`
	scopeChildToolResult = `{"type":"tool_result","tool_use_id":"toolu_scope_child","content":"ok"}`
)

func scopeMessage(kind, parentField, block string) string {
	return fmt.Sprintf(`{"type":%q,"session_id":%q%s,"message":{"role":%q,"content":[%s]}}`, kind, claudeTestUUID, parentField, kind, block)
}

func recordValue(line string) providerjson.Value {
	return providerjson.Value{Reader: strings.NewReader(line), End: int64(len(line))}
}

type scopeCase struct {
	name   string
	lines  []string
	commit bool
}

// childScopeCases states which traces the scoped completion contract accepts.
// A nested message keeps its structural, session, and ordering validation but
// never opens, advances, or settles the root follow-up cycle or batch.
// advisory selects the current revision, whose unknown content block types
// are opaque JSON while known blocks stay validated.
func childScopeCases(mode string, advisory bool) []scopeCase {
	initial := interactiveInitLine(mode)
	principal := attributedResultLine(tracePrincipalText, `,"result_index":0`, 4, "0.1")
	followUp := attributedResultLine("neutral", notificationAttribution(1), 1, "0.2")
	acknowledgement := attributedResultLine("", notificationAttribution(1), 0, "0.2")
	rootText := scopeMessage("assistant", scopeRootParent, scopeTextBlock)
	childAssistant := scopeMessage("assistant", scopeChildParent, scopeChildToolUse)
	childUser := scopeMessage("user", scopeChildParent, scopeChildToolResult)
	childText := scopeMessage("assistant", scopeChildParent, scopeTextBlock)
	cases := []scopeCase{
		{"nested messages before the parent repeats init", []string{
			initial, scopeMessage("assistant", scopeRootParent, scopeAgentToolUse), interactiveAgent, principal,
			childAssistant, childUser, childText, scopeAgentUpdated, interactiveSettled, initial, rootText, followUp,
		}, true},
		{"nested messages alone after the principal", []string{initial, principal, childAssistant}, true},
		{"nested messages inside an open parent cycle", []string{initial, principal, initial, childAssistant, rootText, followUp}, true},
		{"nested message between parent messages", []string{initial, principal, initial, rootText, childAssistant, followUp}, true},
		{"nested messages before the principal", []string{initial, childAssistant, childUser, principal}, true},
		{"null parent scope is root", []string{initial, principal, initial, rootText, followUp}, true},
		{"nested message does not settle an open cycle", []string{initial, principal, initial, childAssistant}, false},
		{"nested message does not close a parent cycle", []string{initial, principal, initial, rootText, childAssistant}, false},
		{"nested message does not settle a batch acknowledgement", []string{initial, principal, acknowledgement, childAssistant}, false},
		{"init after parent message with nested message between", []string{initial, principal, initial, rootText, childAssistant, initial, followUp}, false},
		{"parent message then init", []string{initial, principal, rootText, initial, followUp}, false},
		{"parent message before init", []string{rootText, initial, principal}, false},
		{"nested message before init", []string{childAssistant, initial, principal}, false},
		{"nested message with invalid content after the principal", []string{initial, principal, scopeMessage("assistant", scopeChildParent, `{"type":"unknown"}`)}, advisory},
		{"nested message with invalid content before the principal", []string{initial, scopeMessage("user", scopeChildParent, `{"type":"unknown"}`), principal}, advisory},
		{"nested message with an invalid known block after the principal", []string{initial, principal, scopeMessage("assistant", scopeChildParent, `{"type":"tool_use","id":"toolu_x","input":{}}`)}, false},
		{"nested message with an invalid known block before the principal", []string{initial, scopeMessage("user", scopeChildParent, `{"type":"tool_result","is_error":"no"}`), principal}, false},
		{"nested message from another session", []string{initial, principal, strings.Replace(childAssistant, claudeTestUUID, "123e4567-e89b-12d3-a456-426614174001", 1)}, false},
	}
	for _, scope := range []struct{ name, field string }{
		{"empty string", `,"parent_tool_use_id":""`},
		{"blank string", `,"parent_tool_use_id":"  "`},
		{"number", `,"parent_tool_use_id":7`},
		{"boolean", `,"parent_tool_use_id":true`},
		{"object", `,"parent_tool_use_id":{}`},
		{"array", `,"parent_tool_use_id":[]`},
		{"NUL", `,"parent_tool_use_id":"\u0000"`},
		{"oversized", `,"parent_tool_use_id":"` + strings.Repeat("a", 1025) + `"`},
		{"invalid UTF-8", `,"parent_tool_use_id":"` + "\xff" + `"`},
		{"case alias", `,"Parent_Tool_Use_Id":"toolu_scope_agent"`},
	} {
		bad := scopeMessage("assistant", scope.field, scopeChildToolUse)
		cases = append(cases,
			scopeCase{"invalid scope " + scope.name + " before the principal", []string{initial, bad, principal}, false},
			scopeCase{"invalid scope " + scope.name + " outside a parent cycle", []string{initial, principal, bad}, false},
			scopeCase{"invalid scope " + scope.name + " inside a parent cycle", []string{initial, principal, initial, bad, followUp}, false},
		)
	}
	return cases
}

func TestScopedPrintCompletionRoutesNestedMessages(t *testing.T) {
	for _, revision := range attributedRevisions() {
		for _, mode := range []string{Mode, WorkspaceWriteMode} {
			for _, tc := range childScopeCases(mode, revision.advisory) {
				t.Run(revision.name+"/"+mode+"/"+tc.name, func(t *testing.T) {
					interpretation, answer := evaluateWriteTrace(t, revision.print(mode), tc.lines)
					if revision.name == "follow-up" && tc.commit {
						if interpretation.Verdict != task.VerdictCommitted || answer != expectedResponses(t, tc.lines) {
							t.Fatalf("%+v answer=%q", interpretation, answer)
						}
					} else {
						requireScopeVerdict(t, tc, interpretation, answer)
					}
				})
			}
		}
	}
}

func TestScopedInteractiveSealedCompletionRoutesNestedMessages(t *testing.T) {
	for _, revision := range attributedRevisions() {
		for _, mode := range []string{Mode, WorkspaceWriteMode} {
			for _, tc := range childScopeCases(mode, revision.advisory) {
				t.Run(revision.name+"/"+mode+"/"+tc.name, func(t *testing.T) {
					lines := append(append([]string{interactiveRunning}, tc.lines...), interactiveIdle)
					interpretation, answer := evaluateInteractiveTrace(t, revision.interactive(mode), lines)
					if revision.name == "follow-up" && tc.commit {
						requireCommittedResponses(t, interpretation, answer, lines)
					} else {
						requireScopeVerdict(t, tc, interpretation, answer)
					}
				})
			}
		}
	}
}

func requireScopeVerdict(t *testing.T, tc scopeCase, interpretation task.Interpretation, answer string) {
	t.Helper()
	if tc.commit {
		if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
			t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
		}
		return
	}
	if interpretation.Verdict != task.VerdictRejected || answer != "" {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
	}
}

func TestScopedLiveInteractionRoutesNestedMessages(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			// The first case is the native-shaped nested-agent trace.
			trace := append(append([]string{interactiveRunning}, childScopeCases(mode, true)[0].lines...), interactiveIdle)
			p := liveAttributedInteractionIn(t, mode)
			ready := 0
			for index, line := range trace {
				update := receiveClaudeRecord(t, p, line)
				ready += countInteractionEvents(update, "ready")
				if update.Done != (index == len(trace)-1) {
					t.Fatalf("record %d done=%v", index, update.Done)
				}
			}
			if ready != 1 || p.Complete() != nil {
				t.Fatalf("ready=%d complete=%v", ready, p.Complete())
			}
		})
	}
}

func countInteractionEvents(update execution.InteractionUpdate, kind string) int {
	count := 0
	for _, event := range update.Events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func TestScopedLiveInteractionRejectsInvalidScopeOnReceive(t *testing.T) {
	p := liveAttributedInteractionIn(t, Mode)
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	bad := scopeMessage("assistant", `,"parent_tool_use_id":""`, scopeTextBlock)
	if _, err := p.ReceiveRecord(recordValue(bad)); err == nil {
		t.Fatal("invalid message scope was accepted before the principal result")
	}
}

// TestScopedLiveInteractionKeepsRootQuestionAgainstNestedToolResult proves a
// nested agent's tool_result cannot resolve a root AskUserQuestion that shares
// its tool_use_id; only a root tool_result resolves it.
func TestScopedLiveInteractionKeepsRootQuestionAgainstNestedToolResult(t *testing.T) {
	const question = `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_question","input":{"questions":[{"question":"Choose","options":[{"label":"Blue"}]}]}}}`
	result := `{"type":"tool_result","tool_use_id":"toolu_question","content":"answered"}`
	p := liveAttributedInteractionIn(t, Mode)
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	if update := receiveClaudeRecord(t, p, question); len(update.Events) != 1 || update.Events[0].Kind != "request" {
		t.Fatalf("question was not exposed: %+v", update)
	}
	nested := receiveClaudeRecord(t, p, scopeMessage("user", scopeChildParent, result))
	if len(nested.Events) != 0 || len(p.pending) != 1 {
		t.Fatalf("nested tool_result resolved the root question: events=%+v pending=%d", nested.Events, len(p.pending))
	}
	root := receiveClaudeRecord(t, p, scopeMessage("user", scopeRootParent, result))
	if len(root.Events) != 1 || root.Events[0].Kind != "resolved" || len(p.pending) != 0 {
		t.Fatalf("root tool_result did not resolve the question: events=%+v pending=%d", root.Events, len(p.pending))
	}
}

func TestScopedLiveInteractionRejectsInvalidScopeToolResultWithoutResolving(t *testing.T) {
	const question = `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_question","input":{"questions":[{"question":"Choose","options":[{"label":"Blue"}]}]}}}`
	p := liveAttributedInteractionIn(t, Mode)
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	receiveClaudeRecord(t, p, question)
	bad := scopeMessage("user", `,"parent_tool_use_id":7`, `{"type":"tool_result","tool_use_id":"toolu_question","content":"answered"}`)
	if _, err := p.ReceiveRecord(recordValue(bad)); err == nil || len(p.pending) != 1 {
		t.Fatalf("invalid-scope tool_result err=%v pending=%d", err, len(p.pending))
	}
}
