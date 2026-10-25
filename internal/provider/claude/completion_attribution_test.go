package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Positions in nativeFollowUpTrace. The trace is a sanitized structural copy
// of a sealed native print invocation: one caller turn whose background shell
// work finished after the principal result, followed by two provider-native
// task-notification cycles with repeated same-session init events.
const (
	tracePrincipal      = 8
	traceFirstInit      = 13
	traceFirstFollowUp  = 18
	traceSecondInit     = 20
	traceSecondFollowUp = 23
	tracePrincipalText  = "principal answer"
)

func nativeFollowUpTrace() []string {
	s := claudeTestUUID
	return []string{
		fmt.Sprintf(`{"type":"system","subtype":"hook_started","session_id":%q,"hook_id":"hook-1"}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"hook_response","session_id":%q,"hook_id":"hook-1","outcome":"success"}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"ui_invalidate","session_id":%q,"event":"refresh"}`, s),
		writeInitLine(s),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"true"}}]}}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"task_started","session_id":%q,"task_id":"bg-1","task_type":"local_bash","is_backgrounded":true}`, s),
		fmt.Sprintf(`{"type":"user","session_id":%q,"message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"started"}]}}`, s),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"interim"}]}}`, s),
		attributedResultLine(tracePrincipalText, `,"result_index":0`, 12, "0.4747"),
		fmt.Sprintf(`{"type":"system","subtype":"task_updated","session_id":%q,"task_id":"bg-1","patch":{"status":"completed"}}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","session_id":%q,"task_id":"bg-1","status":"completed"}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"background_tasks_changed","session_id":%q,"tasks":[]}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"ui_invalidate","session_id":%q,"event":"refresh"}`, s),
		writeInitLine(s),
		fmt.Sprintf(`{"type":"system","subtype":"task_updated","session_id":%q,"task_id":"bg-2","patch":{"status":"completed"}}`, s),
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","session_id":%q,"task_id":"bg-2","status":"completed"}`, s),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"thinking","thinking":"neutral"}]}}`, s),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral follow-up"}]}}`, s),
		attributedResultLine("neutral follow-up", notificationAttribution(1), 1, "0.4982"),
		fmt.Sprintf(`{"type":"system","subtype":"ui_invalidate","session_id":%q,"event":"refresh"}`, s),
		writeInitLine(s),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral second follow-up"}]}}`, s),
		fmt.Sprintf(`{"type":"rate_limit_event","session_id":%q,"rate_limit_info":{"status":"allowed"}}`, s),
		attributedResultLine("neutral second follow-up", notificationAttribution(2), 1, "0.5157"),
	}
}

func attributedResultLine(answer, attribution string, numTurns int, cost string) string {
	encoded, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%s,"stop_reason":"end_turn","session_id":%q,"permission_denials":[],"queued_turn_count":0,"terminal_reason":"completed","usage":{"input_tokens":12,"output_tokens":8},"modelUsage":{"claude-test":{"inputTokens":12,"outputTokens":8,"costUSD":%s}},"total_cost_usd":%s,"duration_ms":1500,"num_turns":%d%s}`, encoded, claudeTestUUID, cost, cost, numTurns, attribution)
}

func notificationAttribution(index int) string {
	return fmt.Sprintf(`,"result_index":%d,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-%d"}`, index, index)
}

func withTraceLine(index int, line string) func([]string) []string {
	return func(lines []string) []string {
		lines[index] = line
		return lines
	}
}

func evaluateWriteTrace(t *testing.T, v predicate.Interpreter, lines []string) (task.Interpretation, string) {
	t.Helper()
	stdout := jsonl(lines...)
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	var answer bytes.Buffer
	interpretation, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}

func TestAttributedCompletionPublishesPrincipalAnswerFromNativeFollowUpTrace(t *testing.T) {
	interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), nativeFollowUpTrace())
	if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText || len(interpretation.Diagnostics) != 0 {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
	}
	if interpretation.Session == nil || *interpretation.Session != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) {
		t.Fatalf("session=%+v", interpretation.Session)
	}
	if err := task.ValidateInterpretation(interpretation); err != nil {
		t.Fatal(err)
	}
	if len(interpretation.Usage) != 2 {
		t.Fatalf("usage=%+v", interpretation.Usage)
	}
	main, whole := interpretation.Usage[0], interpretation.Usage[1]
	assertClaudeInt64(t, "principal num_turns", main.NumTurns, 12)
	if main.Scope != task.UsageScopeMainAgent || main.Reliability != task.UsageReliabilityReported {
		t.Fatalf("main usage=%+v", main)
	}
	if whole.Scope != task.UsageScopeWholeTree || whole.EstimatedCostUSD == nil || *whole.EstimatedCostUSD != "0.5157" {
		t.Fatalf("cumulative cost must come from the settled result without summing: %+v", whole)
	}
}

func TestPreAttributionRevisionStillRejectsNativeFollowUpTrace(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := nativeFollowUpTrace()
		if mode == Mode {
			for _, index := range []int{3, traceFirstInit, traceSecondInit} {
				lines[index] = initLine(claudeTestUUID)
			}
		}
		interpretation, answer := evaluateWriteTrace(t, newPreAttributionInterpreter(mode), lines)
		if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != refusalMalformed || answer != "" {
			t.Fatalf("%s pre-attribution replay changed: %+v answer=%q", mode, interpretation, answer)
		}
		current, currentAnswer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
		if current.Verdict != task.VerdictCommitted || currentAnswer != tracePrincipalText {
			t.Fatalf("%s attributed interpretation=%+v answer=%q", mode, current, currentAnswer)
		}
	}
}

func TestAttributedRecoveryReassessesPreAttributionSealWithoutRewriting(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	source := preAttributionReferenceForMode(WorkspaceWriteMode)
	recovery, err := registry.ResolveRecovery(source)
	if err != nil || !recovery.Target().Equal(WorkspaceWriteReference()) {
		t.Fatalf("recovery=%v err=%v", recovery, err)
	}
	stdout := jsonl(nativeFollowUpTrace()...)
	original := bytes.Clone(stdout)
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, source), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	var answer bytes.Buffer
	interpretation, err := recovery.Recover(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != tracePrincipalText {
		t.Fatalf("recovery interpretation=%+v answer=%q err=%v", interpretation, answer.String(), err)
	}
	if !input.Seal.Predicate.Equal(source) || !bytes.Equal(stdout, original) {
		t.Fatal("recovery rewrote the sealed source predicate or evidence")
	}
	previous, ok := newPreAttributionInterpreter(WorkspaceWriteMode).(predicate.RecoveryInterpreter)
	if !ok {
		t.Fatal("pre-attribution interpreter does not expose the recovery contract")
	}
	if _, err := previous.Recover(input, &claudeTestEvidence{stdout: stdout}, &answer); err == nil {
		t.Fatal("pre-attribution revision became a recovery target")
	}
}

func TestAttributedCompletionRejectsUnattributedOrUnsettledCycles(t *testing.T) {
	failedPrincipal := `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"` + claudeTestUUID + `","errors":["failed"],"result_index":0}`
	tests := []struct {
		name   string
		mutate func([]string) []string
		want   string
	}{
		{"principal failed before notification success", withTraceLine(tracePrincipal, failedPrincipal), refusalProviderFailed + ": error_during_execution: failed"},
		{"principal empty before notification answer", withTraceLine(tracePrincipal, attributedResultLine(" ", `,"result_index":0`, 12, "0.4747")), refusalEmpty},
		{"principal from notification", withTraceLine(tracePrincipal, attributedResultLine("x", notificationAttribution(0), 1, "0.1")), refusalMalformed},
		{"principal from peer", withTraceLine(tracePrincipal, attributedResultLine("x", `,"result_index":0,"origin":{"kind":"peer"}`, 1, "0.1")), refusalMalformed},
		{"principal index gap", withTraceLine(tracePrincipal, attributedResultLine("x", `,"result_index":1`, 1, "0.1")), refusalMalformed},
		{"second root result without origin", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1`, 1, "0.1")), refusalMalformed},
		{"follow-up human origin", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1,"origin":{"kind":"human"}`, 1, "0.1")), refusalMalformed},
		{"scheduled trigger", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1,"origin":{"kind":"task-notification","subkind":"scheduled-trigger"}`, 1, "0.1")), refusalMalformed},
		{"peer send message", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1,"origin":{"kind":"task-notification","subkind":"peer-send-message"}`, 1, "0.1")), refusalMalformed},
		{"duplicate index", withTraceLine(traceSecondFollowUp, attributedResultLine("x", notificationAttribution(1), 1, "0.1")), refusalMalformed},
		{"index gap", withTraceLine(traceSecondFollowUp, attributedResultLine("x", notificationAttribution(3), 1, "0.1")), refusalMalformed},
		{"follow-up missing index", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"origin":{"kind":"task-notification"}`, 1, "0.1")), refusalMalformed},
		{"principal missing index", withTraceLine(tracePrincipal, attributedResultLine(tracePrincipalText, "", 12, "0.4747")), refusalMalformed},
		{"string index", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":"1","origin":{"kind":"task-notification"}`, 1, "0.1")), refusalMalformed},
		{"negative index", withTraceLine(tracePrincipal, attributedResultLine("x", `,"result_index":-1`, 1, "0.1")), refusalMalformed},
		{"origin not object", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1,"origin":"task-notification"`, 1, "0.1")), refusalMalformed},
		{"duplicate origin key", withTraceLine(traceFirstFollowUp, attributedResultLine("x", `,"result_index":1,"origin":{"kind":"task-notification","kind":"human"}`, 1, "0.1")), refusalMalformed},
		{"changed follow-up init", withTraceLine(traceFirstInit, strings.Replace(writeInitLine(claudeTestUUID), `"cwd":"/workspace"`, `"cwd":"/elsewhere"`, 1)), refusalMalformed},
		{"changed follow-up model", withTraceLine(traceSecondInit, strings.Replace(writeInitLine(claudeTestUUID), `"model":"claude-test"`, `"model":"claude-other"`, 1)), refusalMalformed},
		{"changed follow-up permission", withTraceLine(traceFirstInit, initLine(claudeTestUUID)), refusalMalformed},
		{"changed follow-up session", withTraceLine(traceSecondInit, writeInitLine("123e4567-e89b-12d3-a456-426614174001")), refusalMalformed},
		{"repeated init in cycle", withTraceLine(traceFirstInit+1, writeInitLine(claudeTestUUID)), refusalMalformed},
		{"init after cycle messages", withTraceLine(traceSecondInit+2, writeInitLine(claudeTestUUID)), refusalMalformed},
		{"trailing message cycle", func(lines []string) []string { return append(lines, lines[traceSecondInit+1]) }, refusalMalformed},
		{"trailing init cycle", func(lines []string) []string { return append(lines, writeInitLine(claudeTestUUID)) }, refusalMalformed},
		{"follow-up failure", withTraceLine(traceFirstFollowUp, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"`+claudeTestUUID+`","errors":["failed"]`+notificationAttribution(1)+`}`), refusalProviderFailed + ": error_during_execution: failed"},
		{"follow-up refusal", withTraceLine(traceFirstFollowUp, strings.Replace(attributedResultLine("x", notificationAttribution(1), 1, "0.1"), `"end_turn"`, `"refusal"`, 1)), refusalRefused},
		{"follow-up session", withTraceLine(traceFirstFollowUp, strings.Replace(attributedResultLine("x", notificationAttribution(1), 1, "0.1"), claudeTestUUID, "123e4567-e89b-12d3-a456-426614174001", 1)), refusalMalformed},
	}
	// The advisory revision compares a repeated init on its authority only.
	advisory := map[string]string{"changed follow-up init": committedPrincipal, "changed follow-up model": committedPrincipal}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireRevisionVerdicts(t, func() []string { return test.mutate(nativeFollowUpTrace()) }, test.want, advisory[test.name])
		})
	}
}

// committedPrincipal marks a case that a revision commits with the principal
// answer.
const committedPrincipal = "committed-principal"

// requireRevisionVerdicts evaluates a trace under the frozen scoped and the
// current advisory workspace-write revisions. want is the exact frozen
// verdict; advisory, when set, is the current revision's different verdict.
func requireRevisionVerdicts(t *testing.T, lines func() []string, want, advisory string) {
	t.Helper()
	if advisory == "" {
		advisory = want
	}
	for _, revision := range []struct {
		interpreter predicate.Interpreter
		want        string
	}{{newScopedInterpreter(WorkspaceWriteMode), want}, {NewWorkspaceWriteInterpreter(), advisory}} {
		interpretation, answer := evaluateWriteTrace(t, revision.interpreter, lines())
		version := revision.interpreter.Reference().Version
		if revision.want == committedPrincipal {
			if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
				t.Fatalf("%s interpretation=%+v answer=%q", version, interpretation, answer)
			}
			continue
		}
		if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != revision.want || answer != "" {
			t.Fatalf("%s interpretation=%+v answer=%q want=%q", version, interpretation, answer, revision.want)
		}
	}
}

func TestAttributedCompletionAcceptsSingleAndBatchedResults(t *testing.T) {
	s := claudeTestUUID
	batchedEmpty := attributedResultLine("", notificationAttribution(1), 0, "0.4747")
	tests := []struct {
		name  string
		lines []string
	}{
		{"legacy single result", []string{writeInitLine(s), resultLine(s, tracePrincipalText)}},
		{"human indexed single result", []string{writeInitLine(s), attributedResultLine(tracePrincipalText, `,"result_index":0,"origin":{"kind":"human"}`, 1, "0.1")}},
		{"batched empty notification", []string{
			writeInitLine(s), attributedResultLine(tracePrincipalText, `,"result_index":0`, 3, "0.4"), batchedEmpty,
			writeInitLine(s), attributedResultLine("neutral", notificationAttribution(2), 1, "0.5"),
			fmt.Sprintf(`{"type":"system","subtype":"status","session_id":%q,"status":"idle"}`, s),
		}},
		{"several acknowledgements before the answering turn", []string{
			writeInitLine(s), attributedResultLine(tracePrincipalText, `,"result_index":0`, 3, "0.4"), batchedEmpty,
			attributedResultLine(" ", notificationAttribution(2), 0, "0.4"), writeInitLine(s),
			fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral"}]}}`, s),
			attributedResultLine("neutral", notificationAttribution(3), 1, "0.5"),
		}},
		{"acknowledgement after the answering turn messages", []string{
			writeInitLine(s), attributedResultLine(tracePrincipalText, `,"result_index":0`, 3, "0.4"), writeInitLine(s),
			fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral"}]}}`, s),
			batchedEmpty, attributedResultLine("neutral", notificationAttribution(2), 1, "0.5"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), test.lines)
			if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
				t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
			}
		})
	}
}

func withResultMetadata(index int, base, old, replacement string) func([]string) []string {
	return withTraceLine(index, strings.Replace(base, old, replacement, 1))
}

func TestCompletionMetadataRejectsAbortedQueuedAndMalformedResults(t *testing.T) {
	principal := attributedResultLine(tracePrincipalText, `,"result_index":0`, 12, "0.4747")
	followUp := attributedResultLine("neutral follow-up", notificationAttribution(1), 1, "0.4982")
	completed := `"terminal_reason":"completed"`
	queued := `"queued_turn_count":0`
	tests := []struct {
		name   string
		mutate func([]string) []string
		want   string
	}{
		{"principal aborted tools", withResultMetadata(tracePrincipal, principal, completed, `"terminal_reason":"aborted_tools"`), refusalProviderFailed + ": terminal_reason=aborted_tools"},
		{"principal aborted streaming", withResultMetadata(tracePrincipal, principal, completed, `"terminal_reason":"aborted_streaming"`), refusalProviderFailed + ": terminal_reason=aborted_streaming"},
		{"follow-up aborted tools", withResultMetadata(traceFirstFollowUp, followUp, completed, `"terminal_reason":"aborted_tools"`), refusalProviderFailed + ": terminal_reason=aborted_tools"},
		{"second follow-up api error", withResultMetadata(traceSecondFollowUp, attributedResultLine("x", notificationAttribution(2), 1, "0.5157"), completed, `"terminal_reason":"api_error"`), refusalProviderFailed + ": terminal_reason=api_error"},
		{"principal undocumented terminal", withResultMetadata(tracePrincipal, principal, completed, `"terminal_reason":"paused"`), refusalMalformed},
		{"principal terminal not string", withResultMetadata(tracePrincipal, principal, completed, `"terminal_reason":1`), refusalMalformed},
		{"principal terminal null", withResultMetadata(tracePrincipal, principal, completed, `"terminal_reason":null`), refusalMalformed},
		{"principal queued human input", withResultMetadata(tracePrincipal, principal, queued, `"queued_turn_count":1`), refusalIncomplete},
		{"follow-up queued human input", withResultMetadata(traceFirstFollowUp, followUp, queued, `"queued_turn_count":2`), refusalIncomplete},
		{"queued negative", withResultMetadata(tracePrincipal, principal, queued, `"queued_turn_count":-1`), refusalMalformed},
		{"queued string", withResultMetadata(traceFirstFollowUp, followUp, queued, `"queued_turn_count":"0"`), refusalMalformed},
		{"queued null", withResultMetadata(tracePrincipal, principal, queued, `"queued_turn_count":null`), refusalMalformed},
		{"principal origin null", withResultMetadata(tracePrincipal, principal, `,"result_index":0`, `,"result_index":0,"origin":null`), refusalMalformed},
		{"result index null", withResultMetadata(tracePrincipal, principal, `,"result_index":0`, `,"result_index":null`), refusalMalformed},
	}
	// The advisory revision reports an undocumented reason as unsupported.
	advisory := map[string]string{"principal undocumented terminal": refusalUnsupportedCompletion}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireRevisionVerdicts(t, func() []string { return test.mutate(nativeFollowUpTrace()) }, test.want, advisory[test.name])
		})
	}
}

func TestEmptyFollowUpsSettleOnlyAsZeroTurnBatchAcknowledgements(t *testing.T) {
	empty := attributedResultLine("", notificationAttribution(1), 1, "0.4982")
	ack := attributedResultLine("", notificationAttribution(3), 0, "0.5157")
	appendAck := func(line string) func([]string) []string {
		return func(lines []string) []string { return append(lines, line) }
	}
	tests := []struct {
		name   string
		mutate func([]string) []string
		want   string
	}{
		{"empty with turns", withTraceLine(traceFirstFollowUp, empty), refusalIncomplete},
		{"blank with turns", withTraceLine(traceFirstFollowUp, attributedResultLine("  ", notificationAttribution(1), 2, "0.4982")), refusalIncomplete},
		{"empty without turns", withResultMetadata(traceFirstFollowUp, empty, `,"num_turns":1`, ""), refusalIncomplete},
		{"empty with malformed turns", withResultMetadata(traceFirstFollowUp, empty, `"num_turns":1`, `"num_turns":"0"`), refusalIncomplete},
		{"missing answer with turns", withResultMetadata(traceFirstFollowUp, empty, `"result":"",`, ""), refusalIncomplete},
		{"acknowledgement at EOF", appendAck(ack), refusalIncomplete},
		{"aborted acknowledgement", appendAck(strings.Replace(ack, `"terminal_reason":"completed"`, `"terminal_reason":"aborted_tools"`, 1)), refusalProviderFailed + ": terminal_reason=aborted_tools"},
		{"acknowledgement inside an open cycle at EOF", withTraceLine(traceSecondFollowUp, attributedResultLine("", notificationAttribution(2), 0, "0.5157")), refusalMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), test.mutate(nativeFollowUpTrace()))
			if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != test.want || answer != "" {
				t.Fatalf("interpretation=%+v answer=%q want=%q", interpretation, answer, test.want)
			}
		})
	}
}

func TestFollowUpUsageFaultsWarnWithoutReplacingPrincipalAnswer(t *testing.T) {
	badUsage := strings.Replace(attributedResultLine("x", notificationAttribution(2), 1, "0.5157"), `"usage":{"input_tokens":12,"output_tokens":8}`, `"usage":"unavailable"`, 1)
	interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), withTraceLine(traceSecondFollowUp, badUsage)(nativeFollowUpTrace()))
	if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText || len(interpretation.Diagnostics) != 1 || interpretation.Diagnostics[0].Code != "optional_usage_unavailable" {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
	}
	for _, usage := range interpretation.Usage {
		if usage.Reliability != task.UsageReliabilityUnreliable {
			t.Fatalf("settled usage fault was not marked unreliable: %+v", usage)
		}
	}
	noCost := strings.Replace(attributedResultLine("x", notificationAttribution(2), 1, "0.5157"), `,"total_cost_usd":0.5157`, "", 1)
	interpretation, _ = evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), withTraceLine(traceSecondFollowUp, noCost)(nativeFollowUpTrace()))
	if interpretation.Verdict != task.VerdictCommitted || slices.ContainsFunc(interpretation.Usage, func(usage task.UsageMetadata) bool { return usage.EstimatedCostUSD != nil }) {
		t.Fatalf("stale principal cumulative cost was reported as the settled total: %+v", interpretation.Usage)
	}
}

func TestAttributedContractVersionsCompletionPolicy(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		current, previous := ContractForMode(mode), PreAttributionNativeContractForMode(mode)
		if !strings.Contains(current, `"completion_attribution":`) || strings.Contains(previous, "completion_attribution") ||
			strings.Contains(current, "one successful result,") || strings.Contains(current, "exactly one result object") {
			t.Fatalf("%s contract does not version completion attribution: %s", mode, current)
		}
		if ReferenceForMode(mode).Equal(preAttributionReferenceForMode(mode)) || ReferenceForMode(mode).Equal(scopedReference(mode)) {
			t.Fatalf("%s attributed reference reused a frozen digest", mode)
		}
	}
	for _, test := range []struct {
		ref  task.PredicateRef
		want string
	}{
		{preAttributionReferenceForMode(Mode), "f1c0aa32cf4b089214f2ac727a5510dde46c923066cccfffb2a912069af3c455"},
		{preAttributionReferenceForMode(WorkspaceWriteMode), "dc2c8fb5cef5a1acc568e890705c28add52596398f731b0142284ef176d2e58a"},
	} {
		if test.ref.SHA256 != test.want {
			t.Fatalf("pre-attribution digest changed: %+v want=%s", test.ref, test.want)
		}
	}
	if ReferenceForMode(Mode).Version != "native-permissions-v5" || ReferenceForMode(WorkspaceWriteMode).Version != "native-permissions-v6" ||
		scopedReference(Mode).Version != "native-permissions-v4" || scopedReference(WorkspaceWriteMode).Version != "native-permissions-v5" {
		t.Fatalf("attributed versions=%+v %+v", ReferenceForMode(Mode), ReferenceForMode(WorkspaceWriteMode))
	}
}

// fixtureClaudeEnvironment installs a fixture claude executable on PATH.
func fixtureClaudeEnvironment(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "claude"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	return directory
}

func TestExistingPreAttributionProfileRetainsRelaxedProbeAndPredicate(t *testing.T) {
	directory := fixtureClaudeEnvironment(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, interactive := range []bool{false, true} {
			request := profileRequest()
			request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
			request.RequestedConfig.Interactive = interactive
			currentReference, frozenReference := ReferenceForMode(mode), preAttributionReferenceForMode(mode)
			if interactive {
				currentReference, frozenReference = interactionReference(mode), preAttributionInteractionReference(mode)
			}
			requireFrozenReconstruction(t, request, currentReference, frozenReference, singleResultCompletion)
		}
	}
}

// requireFrozenReconstruction rebuilds an existing task admitted under
// frozenReference and requires its predicate, launch contract, runtime probe,
// and (for interactive tasks) live completion policy to be the frozen ones.
func requireFrozenReconstruction(t *testing.T, request task.TaskRecord, currentReference, frozenReference task.PredicateRef, frozenPolicy completionPolicy) {
	t.Helper()
	fresh, err := PrepareCandidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: fresh.Inspection.Executable, SHA256: fresh.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fresh.Finalize(facts, time.Now())
	if err != nil || !current.Plan.Predicate.Equal(currentReference) {
		t.Fatalf("fresh predicate=%+v err=%v", current.Plan.Predicate, err)
	}
	existing, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: frozenReference, EffectiveConfig: current.Effective, Environment: current.Plan.Environment, EnvironmentRecorded: true}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if requireRuntimeProbeDefinition(t, existing.Inspection, []string{"--print"}) != requireRuntimeProbeDefinition(t, fresh.Inspection, []string{"--print"}) {
		t.Fatalf("%s reconstruction changed its runtime probe", frozenReference.Version)
	}
	rebuilt, err := existing.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	requireRebuiltHistoricalProfile(t, frozenReference, current, rebuilt)
	if request.RequestedConfig.Interactive {
		requireLiveCompletionPolicy(t, current, advisoryScopedCompletion)
		requireLiveCompletionPolicy(t, rebuilt, frozenPolicy)
	}
}

func liveCompletionPolicy(live *claudeInteraction) completionPolicy {
	switch attribution := live.state.attribution; {
	case attribution == nil:
		return singleResultCompletion
	case attribution.advisory:
		return advisoryScopedCompletion
	case attribution.scoped:
		return scopedAttributedCompletion
	default:
		return unscopedAttributedCompletion
	}
}

func requireLiveCompletionPolicy(t *testing.T, profile commonprovider.PreparedProfile, want completionPolicy) {
	t.Helper()
	if profile.Interaction == nil {
		t.Fatal("interactive profile has no live protocol")
	}
	protocol, err := profile.Interaction(task.SessionExpectation{Required: true, ID: claudeTestUUID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong live protocol")
	}
	if got := liveCompletionPolicy(live); got != want {
		t.Fatalf("%s live completion policy=%d want %d", profile.Plan.Predicate.Version, got, want)
	}
}
