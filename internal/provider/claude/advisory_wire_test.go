package claude

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// attributedRevision pairs the print and interactive interpreters of one
// scoped attribution revision family under test.
type attributedRevision struct {
	name        string
	advisory    bool
	print       func(mode string) predicate.Interpreter
	interactive func(mode string) predicate.Interpreter
}

// attributedRevisions are the frozen scoped revisions and the current
// advisory revisions that share the scoped reducer.
func attributedRevisions() []attributedRevision {
	return []attributedRevision{
		{name: "scoped", print: newScopedInterpreter, interactive: newScopedInteractionInterpreter},
		{name: "advisory", advisory: true, print: newAdvisoryInterpreter, interactive: newAdvisoryInteractionInterpreter},
		{name: "follow-up", advisory: true, print: func(mode string) predicate.Interpreter { return NewInterpreter(mode) }, interactive: newInteractionInterpreter},
	}
}

func revisionInput(stdout []byte, reference task.PredicateRef) predicate.Input {
	return predicate.Input{Seal: claudeTestSealForPredicate(stdout, reference), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
}

func evaluateRevisionStdout(t *testing.T, v predicate.Interpreter, stdout []byte) (task.Interpretation, string) {
	t.Helper()
	var answer bytes.Buffer
	interpretation, err := v.Evaluate(revisionInput(stdout, v.Reference()), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}

func liveInteractionWithPolicy(t *testing.T, mode string, policy completionPolicy) *claudeInteraction {
	t.Helper()
	request := task.TaskRecord{RootID: claudeTestRootID, TaskID: claudeTestTaskID, Mode: mode}
	protocol, err := newClaudeInteractionWithCapture(request, task.SessionExpectation{Required: true, ID: claudeTestUUID}, nil, true, policy)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong protocol")
	}
	return p
}

// requireLiveCurrentSettles feeds a session-wrapped caller turn to the current
// live protocol and requires one closure at its end and a clean completion.
func requireLiveCurrentSettles(t *testing.T, mode string, lines []string) {
	t.Helper()
	p := liveInteractionWithPolicy(t, mode, advisoryScopedCompletion)
	trace := wrapInteractiveSession(lines)
	for index, line := range trace {
		update, err := p.ReceiveRecord(recordValue(line))
		if err != nil {
			t.Fatalf("live record %d: %v", index, err)
		}
		if update.Done != (index == len(trace)-1) {
			t.Fatalf("live record %d done=%v", index, update.Done)
		}
	}
	if err := p.Complete(); err != nil {
		t.Fatalf("live completion: %v", err)
	}
}

// requireLiveCurrentFaults requires the current live protocol to refuse a
// protocol, identity, or syntax fault on receipt. A complete native negative
// completion is not such a fault: it settles the transport through
// requireLiveCurrentSettles and its sealed predicate rejects it.
func requireLiveCurrentFaults(t *testing.T, mode string, lines []string) {
	t.Helper()
	if requireLiveError(liveInteractionWithPolicy(t, mode, advisoryScopedCompletion), wrapInteractiveSession(lines)) == nil {
		t.Fatal("live protocol accepted every record of a faulted caller turn")
	}
}

func minimalInitLine(mode string, extra string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"permissionMode":%q%s}`, claudeTestUUID, nativeDefaultPermission(mode), extra)
}

func principalLine() string {
	return attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1")
}

type wireCase struct {
	name  string
	lines []string
	// want is the current revision's refusal; empty commits the principal.
	want string
	// frozenRejects marks a trace the frozen scoped revision rejects.
	frozenRejects bool
}

func requireCurrentWireVerdicts(t *testing.T, mode string, tc wireCase) {
	t.Helper()
	printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), tc.lines)
	interactive, interactiveAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), wrapInteractiveSession(tc.lines))
	if tc.want == "" {
		requireCommittedPrincipal(t, "print", printed, printedAnswer)
		requireCommittedPrincipal(t, "interactive", interactive, interactiveAnswer)
		requireLiveCurrentSettles(t, mode, tc.lines)
	} else {
		if printed.Verdict != task.VerdictRejected || printed.Refusal != tc.want || printedAnswer != "" {
			t.Fatalf("print interpretation=%+v answer=%q want %q", printed, printedAnswer, tc.want)
		}
		// Interactive replay reports a malformed stream as incomplete.
		if interactive.Verdict != task.VerdictRejected || interactiveAnswer != "" {
			t.Fatalf("interactive interpretation=%+v answer=%q", interactive, interactiveAnswer)
		}
		requireLiveCurrentFaults(t, mode, tc.lines)
	}
	frozen, _ := evaluateWriteTrace(t, newScopedInterpreter(mode), tc.lines)
	if (frozen.Verdict == task.VerdictRejected) != (tc.frozenRejects || tc.want != "") {
		t.Fatalf("frozen scoped interpretation=%+v", frozen)
	}
}

func requireCommittedPrincipal(t *testing.T, label string, interpretation task.Interpretation, answer string) {
	t.Helper()
	if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText || interpretation.Session == nil || interpretation.Session.ConversationID != claudeTestUUID {
		t.Fatalf("%s interpretation=%+v answer=%q", label, interpretation, answer)
	}
	if err := task.ValidateInterpretation(interpretation); err != nil {
		t.Fatalf("%s interpretation is invalid: %v", label, err)
	}
}

func TestCurrentInitAuthorityIsSessionAndPermission(t *testing.T) {
	var tools strings.Builder
	for index := 0; index <= maxCurrentToolCount; index++ {
		fmt.Fprintf(&tools, "%q,", fmt.Sprintf("provider-tool-%d", index))
	}
	other := "123e4567-e89b-12d3-a456-426614174001"
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		permission := fmt.Sprintf(`"permissionMode":%q`, nativeDefaultPermission(mode))
		full := interactiveInitLine(mode)
		cases := []wireCase{
			{name: "future metadata", lines: []string{mutateLine(full, `"cwd"`, `"output_style":{"name":"x","extra":[1,{"a":null}]},"plugins":[{"name":"p"}],"cwd"`), principalLine()}},
			{name: "missing advisory metadata", lines: []string{minimalInitLine(mode, ""), principalLine()}, frozenRejects: true},
			{name: "malformed advisory metadata", lines: []string{minimalInitLine(mode, `,"claude_code_version":7,"apiKeySource":null,"cwd":[],"tools":"Read","mcp_servers":{},"model":12`), principalLine()}, frozenRejects: true},
			{name: "ambiguous advisory metadata", lines: []string{minimalInitLine(mode, `,"tools":["Read"],"Tools":["Read"],"cwd":"/a","CWD":"/b","model":"claude-test","Model":"claude-other"`), principalLine()}, frozenRejects: true},
			{name: "ambiguous unconsumed init controls", lines: []string{minimalInitLine(mode, `,"state":1,"state":2,"task_id":"a","Task_Id":"b","task_type":7,"TASK_TYPE":8,"patch":{},"Patch":[]`), principalLine()}, frozenRejects: true},
			{name: "unbounded tool inventory", lines: []string{minimalInitLine(mode, `,"tools":[`+tools.String()+`"last"]`), principalLine()}, frozenRejects: true},
			{name: "missing session", lines: []string{strings.Replace(minimalInitLine(mode, ""), fmt.Sprintf(`"session_id":%q,`, claudeTestUUID), "", 1), principalLine()}, want: refusalMalformed},
			{name: "other session", lines: []string{strings.Replace(full, claudeTestUUID, other, 1), principalLine()}, want: refusalMalformed},
			{name: "missing permission", lines: []string{strings.Replace(full, ","+permission, "", 1), principalLine()}, want: refusalMalformed},
			{name: "other permission", lines: []string{strings.Replace(full, permission, `"permissionMode":"default"`, 1), principalLine()}, want: refusalMalformed},
			{name: "case-aliased permission", lines: []string{strings.Replace(full, permission, permission+","+strings.Replace(permission, "permissionMode", "PermissionMode", 1), 1), principalLine()}, want: refusalMalformed},
			{name: "case-aliased session", lines: []string{mutateLine(full, `"cwd"`, fmt.Sprintf(`"Session_Id":%q,"cwd"`, claudeTestUUID)), principalLine()}, want: refusalMalformed},
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) { requireCurrentWireVerdicts(t, mode, tc) })
		}
	}
}

// TestCurrentSystemSelectsOnlyItsSubtypeControls proves each system subtype
// selects only the controls its consumer reads: an ambiguous control of
// another subtype stays opaque, while the consumed control and the session
// stay strict, including case-folded aliases.
func TestCurrentSystemSelectsOnlyItsSubtypeControls(t *testing.T) {
	system := func(subtype, extra string) string {
		return fmt.Sprintf(`{"type":"system","subtype":%q,"session_id":%q%s}`, subtype, claudeTestUUID, extra)
	}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		cases := []wireCase{
			{name: "unknown subtype with ambiguous controls", lines: []string{system("hook_response", `,"state":"idle","State":"running","permissionMode":"a","PermissionMode":"b"`), initial, principalLine()}, frozenRejects: true},
			{name: "session state with ambiguous task controls", lines: []string{initial, system("session_state_changed", `,"state":"running","task_id":"a","TASK_ID":"b"`), principalLine()}, frozenRejects: true},
			{name: "task notification with ambiguous session state", lines: []string{initial, system("task_notification", `,"task_id":"agent-1","state":1,"STATE":2`), principalLine()}, frozenRejects: true},
			{name: "case-aliased session state", lines: []string{initial, system("session_state_changed", `,"state":"running","State":"idle"`), principalLine()}, want: refusalMalformed},
			{name: "case-aliased started task id", lines: []string{initial, system("task_started", `,"task_type":"local_agent","task_id":"a","Task_Id":"b"`), principalLine()}, want: refusalMalformed},
			{name: "case-aliased started task type", lines: []string{initial, system("task_started", `,"task_type":"local_agent","Task_Type":"other","task_id":"a"`), principalLine()}, want: refusalMalformed},
			{name: "case-aliased notification task id", lines: []string{initial, system("task_notification", `,"task_id":"a","TASK_ID":"b"`), principalLine()}, want: refusalMalformed},
			{name: "case-aliased task patch", lines: []string{initial, system("task_updated", `,"task_id":"a","patch":{"status":"completed"},"Patch":{}`), principalLine()}, want: refusalMalformed},
			{name: "case-aliased session on a control", lines: []string{initial, system("session_state_changed", fmt.Sprintf(`,"state":"running","Session_Id":%q`, claudeTestUUID)), principalLine()}, want: refusalMalformed},
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) { requireCurrentWireVerdicts(t, mode, tc) })
		}
	}
}

// TestCurrentInitRetainsOnlyAValidModel proves the only retained advisory
// init field reaches usage when valid and is dropped, never defaulted, when
// it is missing, malformed, or ambiguous.
func TestCurrentInitRetainsOnlyAValidModel(t *testing.T) {
	for extra, want := range map[string]string{
		`,"model":"claude-test"`:   "claude-test",
		"":                         "",
		`,"model":" claude-test "`: "",
		`,"model":7`:               "",
		`,"model":"claude-test","Model":"claude-other"`: "",
	} {
		interpretation, answer := evaluateWriteTrace(t, NewInterpreter(Mode), []string{minimalInitLine(Mode, extra), principalLine()})
		requireCommittedPrincipal(t, extra, interpretation, answer)
		model := interpretation.Usage[0].Model
		if (want == "") != (model == nil) || (model != nil && *model != want) {
			t.Fatalf("%s usage model=%v want %q", extra, model, want)
		}
	}
}

// TestCurrentRepeatedInitComparesAuthorityOnly replays the native follow-up
// trace with telemetry changes in every repeated init: the principal answer
// and the settled cumulative cost stay exact, while a changed session or
// native permission still rejects.
func TestCurrentRepeatedInitComparesAuthorityOnly(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		changed := []string{
			mutateLine(interactiveInitLine(mode), `"model":"claude-test"`, `"model":"claude-other","claude_code_version":"9.9.9","apiKeySource":"oauth","future":{"a":1,"A":2}`),
			mutateLine(interactiveInitLine(mode), `"cwd":"/workspace"`, `"cwd":"/elsewhere","tools":[],"mcp_servers":[{"name":"m"}]`),
			minimalInitLine(mode, ""),
		}
		for index, replacement := range changed {
			lines := followUpTraceForMode(mode)
			lines[traceSecondInit] = replacement
			lines[traceFirstInit] = changed[(index+1)%len(changed)]
			interpretation, answer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
			requireCommittedResponses(t, interpretation, answer, lines)
			if whole := interpretation.Usage[1]; whole.EstimatedCostUSD == nil || *whole.EstimatedCostUSD != "0.5157" {
				t.Fatalf("%s settled cost=%+v", mode, whole)
			}
			if frozen, _ := evaluateWriteTrace(t, newScopedInterpreter(mode), lines); frozen.Verdict != task.VerdictRejected || frozen.Refusal != refusalMalformed {
				t.Fatalf("%s frozen scoped revision accepted changed init telemetry: %+v", mode, frozen)
			}
			interactive := interactiveFollowUpTrace(mode)
			interactive[7] = replacement
			sealed, sealedAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), interactive)
			requireCommittedResponses(t, sealed, sealedAnswer, interactive)
			frozenSealed, frozenAnswer := evaluateInteractiveTrace(t, newAdvisoryInteractionInterpreter(mode), interactive)
			requireCommittedPrincipal(t, mode+" frozen interactive", frozenSealed, frozenAnswer)
			requireLiveCurrentTrace(t, mode, interactive)
		}
		for name, replacement := range map[string]string{
			"session":    strings.Replace(interactiveInitLine(mode), claudeTestUUID, "123e4567-e89b-12d3-a456-426614174001", 1),
			"permission": interactiveInitLine(otherMode(mode)),
		} {
			lines := followUpTraceForMode(mode)
			lines[traceSecondInit] = replacement
			if rejected, answer := evaluateWriteTrace(t, NewInterpreter(mode), lines); rejected.Verdict != task.VerdictRejected || rejected.Refusal != refusalMalformed || answer != "" {
				t.Fatalf("%s changed %s was accepted: %+v", mode, name, rejected)
			}
			interactive := interactiveFollowUpTrace(mode)
			interactive[7] = replacement
			p := liveInteractionWithPolicy(t, mode, advisoryScopedCompletion)
			if requireLiveError(p, interactive) == nil {
				t.Fatalf("%s live protocol accepted a changed %s", mode, name)
			}
		}
	}
}

// requireLiveCurrentTrace feeds an already session-shaped interactive trace to
// the current live protocol and requires closure at its end.
func requireLiveCurrentTrace(t *testing.T, mode string, trace []string) {
	t.Helper()
	p := liveInteractionWithPolicy(t, mode, advisoryScopedCompletion)
	for index, line := range trace {
		update := receiveClaudeRecord(t, p, line)
		if update.Done != (index == len(trace)-1) {
			t.Fatalf("live record %d done=%v", index, update.Done)
		}
	}
	if err := p.Complete(); err != nil {
		t.Fatalf("live completion: %v", err)
	}
}

func requireLiveError(p *claudeInteraction, trace []string) error {
	for _, line := range trace {
		if _, err := p.ReceiveRecord(recordValue(line)); err != nil {
			return err
		}
	}
	return nil
}

func TestCurrentWireKeepsOpaquePayloadsOpaque(t *testing.T) {
	opaque := strings.Repeat("x", task.MaxControlRecordSize+4096)
	s := claudeTestUUID
	assistant := func(content string) string {
		return fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"role":"assistant","content":[%s]}}`, s, content)
	}
	principal := principalLine()
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		cases := []wireCase{
			{name: "unknown content block", lines: []string{initial, assistant(`{"type":"server_tool_use_v9","payload":{"same":1,"SAME":2}}`), principal}, frozenRejects: true},
			{name: "large opaque tool input with duplicate keys", lines: []string{initial, assistant(fmt.Sprintf(`{"type":"tool_use","id":"tool-1","name":"provider-tool","input":{"opaque":%q,"same":1,"SAME":2,"same":3}}`, opaque)), principal}, frozenRejects: true},
			{name: "duplicate keys in tool result content", lines: []string{initial, fmt.Sprintf(`{"type":"user","session_id":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"text","text":"a","text":"b"}]}]}}`, s), principal}, frozenRejects: true},
			{name: "duplicate top-level extension", lines: []string{initial, mutateLine(assistant(`{"type":"text","text":"x"}`), `"message"`, `"metadata":{"k":1},"Metadata":{"k":2},"message"`), principal}, frozenRejects: true},
			{name: "duplicate result extension", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"fast_mode_state":"off","Fast_Mode_State":"on","stop_reason"`)}, frozenRejects: true},
			{name: "duplicate system telemetry", lines: []string{fmt.Sprintf(`{"type":"system","subtype":"hook_response","session_id":%q,"output":"a","OUTPUT":"b"}`, s), initial, principal}, frozenRejects: true},
			{name: "case-aliased event type", lines: []string{initial, mutateLine(assistant(`{"type":"text","text":"x"}`), `"session_id"`, `"Type":"assistant","session_id"`), principal}, want: refusalMalformed},
			{name: "case-aliased message session", lines: []string{initial, mutateLine(assistant(`{"type":"text","text":"x"}`), `"message"`, fmt.Sprintf(`"Session_Id":%q,"message"`, s)), principal}, want: refusalMalformed},
			{name: "case-aliased content block type", lines: []string{initial, assistant(`{"type":"text","Type":"text","text":"x"}`), principal}, want: refusalMalformed},
			{name: "case-aliased tool name", lines: []string{initial, assistant(`{"type":"tool_use","id":"tool-1","name":"a","NAME":"b","input":{}}`), principal}, want: refusalMalformed},
			{name: "invalid known block", lines: []string{initial, assistant(`{"type":"tool_use","id":"tool-1","input":{}}`), principal}, want: refusalMalformed},
			{name: "case-aliased result answer", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"RESULT":"other","stop_reason"`)}, want: refusalMalformed},
			{name: "case-aliased is_error", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"Is_Error":false,"stop_reason"`)}, want: refusalMalformed},
			{name: "case-aliased result_index", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"Result_Index":0,"stop_reason"`)}, want: refusalMalformed},
			{name: "case-aliased stop_reason", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"Stop_Reason":"end_turn","stop_reason"`)}, want: refusalMalformed},
			{name: "case-aliased terminal_reason", lines: []string{initial, mutateLine(principal, `"stop_reason"`, `"Terminal_Reason":"completed","stop_reason"`)}, want: refusalMalformed},
			{name: "case-aliased origin kind", lines: []string{initial, attributedResultLine(tracePrincipalText, `,"result_index":0,"origin":{"kind":"human","Kind":"task-notification"}`, 1, "0.1")}, want: refusalMalformed},
		}
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) { requireCurrentWireVerdicts(t, mode, tc) })
		}
	}
}

// TestCurrentHostControlRecordsStayUnique proves the interactive host still
// rejects an ambiguous question input it would echo back to the provider.
func TestCurrentHostControlRecordsStayUnique(t *testing.T) {
	question := `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_question","input":{"questions":[{"question":"Choose","options":[{"label":"Blue"}]}],"meta":{"a":1,"A":2}}}}`
	p := liveInteractionWithPolicy(t, Mode, advisoryScopedCompletion)
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	if _, err := p.ReceiveRecord(recordValue(question)); err == nil || len(p.pending) != 0 {
		t.Fatalf("ambiguous question input accepted: err=%v pending=%d", err, len(p.pending))
	}
	unique := strings.Replace(question, `,"meta":{"a":1,"A":2}`, "", 1)
	if update := receiveClaudeRecord(t, p, unique); len(update.Events) != 1 || update.Events[0].Kind != "request" {
		t.Fatalf("question was not exposed: %+v", update)
	}
}

func TestCurrentTaskStartedWithoutTypeIsUntracked(t *testing.T) {
	untyped := `{"type":"system","subtype":"task_started","task_id":"bg-untyped"}`
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := []string{interactiveInitLine(mode), untyped, principalLine()}
		requireLiveCurrentSettles(t, mode, lines)
		sealed, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), wrapInteractiveSession(lines))
		requireCommittedPrincipal(t, mode+" interactive", sealed, answer)
		if frozen, _ := evaluateInteractiveTrace(t, newScopedInteractionInterpreter(mode), wrapInteractiveSession(lines)); frozen.Verdict != task.VerdictRejected {
			t.Fatalf("%s frozen interactive-v5 accepted a task without type: %+v", mode, frozen)
		}
		if requireLiveError(liveInteractionWithPolicy(t, mode, scopedAttributedCompletion), wrapInteractiveSession(lines)) == nil {
			t.Fatalf("%s frozen live protocol accepted a task without type", mode)
		}
		mistyped := []string{interactiveInitLine(mode), `{"type":"system","subtype":"task_started","task_id":"bg","task_type":7}`, principalLine()}
		if requireLiveError(liveInteractionWithPolicy(t, mode, advisoryScopedCompletion), wrapInteractiveSession(mistyped)) == nil {
			t.Fatalf("%s live protocol accepted a mistyped task_type", mode)
		}
		// A tracked local agent still keeps the input open until it settles.
		tracked := liveInteractionWithPolicy(t, mode, advisoryScopedCompletion)
		for _, line := range []string{interactiveRunning, interactiveInitLine(mode), interactiveAgent, principalLine(), interactiveIdle} {
			if update := receiveClaudeRecord(t, tracked, line); update.Done {
				t.Fatalf("%s input closed before the tracked agent settled", mode)
			}
		}
	}
}
