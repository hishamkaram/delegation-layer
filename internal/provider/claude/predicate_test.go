package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	claudeTestSpecHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	claudeTestMetaHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func TestReferenceBindsImmutableContract(t *testing.T) {
	ref := Reference()
	if ref.Adapter != Provider || ref.Mode != Mode || ref.Version != predicateVersion || ref.SHA256 != ContractDigest() {
		t.Fatalf("reference=%+v", ref)
	}
	if !strings.HasSuffix(Contract(), "\n") || task.ComputeSHA256([]byte(Contract())) != ref.SHA256 {
		t.Fatal("contract digest does not cover canonical bytes")
	}
	for _, id := range []string{claudeTestUUID, strings.ToUpper(claudeTestUUID)} {
		if !validSessionID(id) {
			t.Fatalf("valid UUID rejected: %q", id)
		}
	}
	for _, id := range []string{"", "latest", "123e4567-e89b-12d3-a456-42661417400z", "123e4567e89b12d3a456426614174000"} {
		if validSessionID(id) {
			t.Fatalf("invalid UUID accepted: %q", id)
		}
	}
}

func TestSuccessSelectsOnlyTopLevelResultAndPreservesAnswerBytes(t *testing.T) {
	sessionID, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	answerText := " final\nanswer ☃ "
	stdout := jsonl(
		initLine(sessionID),
		fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":"subagent-1","session_id":%q,"message":{"content":[{"type":"text","text":"nested text is not an answer"}]}}`, sessionID),
		`{"type":"telemetry.snapshot","payload":{"large":"ignored"}}`,
		resultLine(sessionID, answerText),
		`{"type":"system","subtype":"status","status":"finished"}`,
	)
	var answer bytes.Buffer
	interpretation, err := NewInterpreter().Evaluate(claudeInput(stdout), &claudeTestEvidence{stdout: stdout, stderr: []byte("diagnostic\n")}, &answer)
	if err != nil {
		t.Fatal(err)
	}
	assertClaudeSuccess(t, interpretation, answer.String(), answerText, sessionID)
}

func assertClaudeSuccess(t *testing.T, interpretation task.Interpretation, answer, wantAnswer, sessionID string) {
	t.Helper()
	if interpretation.Verdict != task.VerdictCommitted || interpretation.Refusal != "" || answer != wantAnswer {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
	}
	wantSession := task.SessionIdentity{Provider: Provider, ConversationID: sessionID}
	if interpretation.Session == nil || *interpretation.Session != wantSession {
		t.Fatalf("session=%+v want=%+v", interpretation.Session, wantSession)
	}
	assertClaudeUsage(t, interpretation.Usage)
	if err := task.ValidateInterpretation(interpretation); err != nil {
		t.Fatalf("interpretation validation: %v", err)
	}
}

func assertClaudeUsage(t *testing.T, usage []task.UsageMetadata) {
	t.Helper()
	if len(usage) != 3 {
		t.Fatalf("usage=%+v", usage)
	}
	mainUsage := usage[0]
	if mainUsage.Scope != task.UsageScopeMainAgent || mainUsage.Source != task.UsageSourceProviderEnvelope || mainUsage.Reliability != task.UsageReliabilityReported || mainUsage.Model == nil || *mainUsage.Model != "claude-test" {
		t.Fatalf("main usage=%+v", mainUsage)
	}
	assertClaudeInt64(t, "input", mainUsage.InputTokens, 12)
	assertClaudeInt64(t, "cache read", mainUsage.CacheReadTokens, 3)
	assertClaudeInt64(t, "cache write", mainUsage.CacheCreationTokens, 2)
	assertClaudeInt64(t, "output", mainUsage.OutputTokens, 8)
	if mainUsage.TotalTokens != nil {
		t.Fatalf("provider did not report a total but one was synthesized: %+v", mainUsage)
	}
	if mainUsage.DurationSeconds == nil || *mainUsage.DurationSeconds != "1.5" {
		t.Fatalf("duration=%+v", mainUsage.DurationSeconds)
	}
	assertClaudeWholeTreeUsage(t, usage)
}

func assertClaudeWholeTreeUsage(t *testing.T, usage []task.UsageMetadata) {
	t.Helper()
	if usage[1].Scope != task.UsageScopeWholeTree || usage[1].EstimatedCostUSD == nil || *usage[1].EstimatedCostUSD != "0.0012" {
		t.Fatalf("aggregate usage=%+v", usage[1])
	}
	if usage[2].Model == nil || *usage[2].Model != "claude-test" || usage[2].Scope != task.UsageScopeWholeTree {
		t.Fatalf("model usage=%+v", usage[2])
	}
}

func TestUnknownTelemetryIsToleratedButUnknownLifecycleRejects(t *testing.T) {
	sessionID := claudeTestUUID
	accepted := jsonl(initLine(sessionID), `{"type":"status.snapshot","payload":{"attempt":1}}`, resultLine(sessionID, "accepted"), fmt.Sprintf(`{"type":"system","subtype":"status","session_id":%q}`, sessionID))
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(accepted, sessionID), &claudeTestEvidence{stdout: accepted}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "accepted" {
		t.Fatalf("telemetry interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
	for _, lifecycle := range []string{"turn.stopped", "thread.finished", "session.aborted"} {
		t.Run(lifecycle, func(t *testing.T) {
			stdout := jsonl(initLine(sessionID), resultLine(sessionID, "ambiguous"), fmt.Sprintf(`{"type":%q}`, lifecycle))
			var answer bytes.Buffer
			nestedInterp, nestedErr := NewInterpreter().Evaluate(claudeContinuationInput(stdout, sessionID), &claudeTestEvidence{stdout: stdout}, &answer)
			if nestedErr != nil || nestedInterp.Verdict != task.VerdictRejected || nestedInterp.Refusal != refusalMalformed || answer.Len() != 0 {
				t.Fatalf("lifecycle interpretation=%+v answer=%q err=%v", nestedInterp, answer.String(), nestedErr)
			}
		})
	}
	conflictingSystem := jsonl(initLine(sessionID), resultLine(sessionID, "answer"), `{"type":"system","subtype":"status","session_id":"123e4567-e89b-12d3-a456-426614174001"}`)
	var conflictingAnswer bytes.Buffer
	interp, err = NewInterpreter().Evaluate(claudeContinuationInput(conflictingSystem, sessionID), &claudeTestEvidence{stdout: conflictingSystem}, &conflictingAnswer)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
		t.Fatalf("conflicting system interpretation=%+v err=%v", interp, err)
	}
}

func TestInitPolicyAndExactFieldNamesAreRequired(t *testing.T) {
	cases := []struct {
		name string
		init string
		want string
	}{
		{name: "permission mode", init: strings.Replace(initLine(claudeTestUUID), `"permissionMode":"dontAsk"`, `"permissionMode":"default"`, 1), want: refusalMalformed},
		{name: "wrong case type", init: strings.Replace(initLine(claudeTestUUID), `"type"`, `"Type"`, 1), want: refusalMalformed},
		{name: "wrong case session", init: strings.Replace(initLine(claudeTestUUID), `"session_id"`, `"Session_ID"`, 1), want: refusalMalformed},
		{name: "wrong case api key source", init: strings.Replace(initLine(claudeTestUUID), `"apiKeySource"`, `"apikeysource"`, 1), want: refusalMalformed},
		{name: "wrong case is_error", init: initLine(claudeTestUUID), want: refusalMalformed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stdout := jsonl(testCase.init, resultLine(claudeTestUUID, "answer"))
			if testCase.name == "wrong case is_error" {
				stdout = jsonl(initLine(claudeTestUUID), strings.Replace(resultLine(claudeTestUUID, "answer"), `"is_error"`, `"Is_Error"`, 1))
			}
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != testCase.want || answer.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
}

func TestNativeInterpreterAcceptsProviderOwnedPolicyFields(t *testing.T) {
	init := strings.Replace(initLine(claudeTestUUID), `"apiKeySource":"none"`, `"apiKeySource":"oauth"`, 1)
	init = strings.Replace(init, `"tools":["Read","Glob","Grep"]`, `"tools":["Read","Glob","Grep","Write","Bash"]`, 1)
	init = strings.Replace(init, `"mcp_servers":[]`, `"mcp_servers":[{"name":"native-server","status":"connected"}]`, 1)
	toolUse := fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"tool_use","id":"native-tool","name":"Bash","input":{"command":"true"}}]}}`, claudeTestUUID)
	stdout := jsonl(init, toolUse, resultLine(claudeTestUUID, "answer"))
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "answer" {
		t.Fatalf("native provider-owned fields were rejected: interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestChangedCLIReportedVersionIsAccepted(t *testing.T) {
	stdout := jsonl(strings.Replace(initLine(claudeTestUUID), Version, "9.9.9", 1), resultLine(claudeTestUUID, "answer"))
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "answer" {
		t.Fatalf("changed CLI version was rejected: interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestWorkspaceWriteInterpreterAcceptsNativeWriteProfile(t *testing.T) {
	stdout := jsonl(writeInitLine(claudeTestUUID), resultLine(claudeTestUUID, "edited"))
	var answer bytes.Buffer
	seal := claudeTestSealForPredicate(stdout, WorkspaceWriteReference())
	input := predicate.Input{Seal: seal, ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	interp, err := NewWorkspaceWriteInterpreter().Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "edited" {
		t.Fatalf("workspace-write interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestWorkspaceWriteInterpreterRejectsReadOnlyInit(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer"))
	seal := claudeTestSealForPredicate(stdout, WorkspaceWriteReference())
	interp, err := NewWorkspaceWriteInterpreter().Evaluate(predicate.Input{Seal: seal, ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}, &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
		t.Fatalf("read-only init accepted by workspace interpreter=%+v err=%v", interp, err)
	}
}

func TestWorkspaceWriteInterpreterAcceptsEditToolEvent(t *testing.T) {
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Edit","input":{"file_path":"/workspace/file.txt","old_string":"before","new_string":"after"}}]},"session_id":"` + claudeTestUUID + `"}`
	stdout := jsonl(writeInitLine(claudeTestUUID), edit, resultLine(claudeTestUUID, "edited"))
	var answer bytes.Buffer
	seal := claudeTestSealForPredicate(stdout, WorkspaceWriteReference())
	interp, err := NewWorkspaceWriteInterpreter().Evaluate(predicate.Input{Seal: seal, ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "edited" {
		t.Fatalf("workspace edit event interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestResultErrorsRefusalLimitsAndMissingTerminalReject(t *testing.T) {
	for _, subtype := range []string{resultErrorDuringExecution, resultErrorMaxTurns, resultErrorMaxBudget, resultErrorMaxStructuredOutputRetries} {
		t.Run(subtype, func(t *testing.T) {
			stdout := jsonl(initLine(claudeTestUUID), errorResultLine(claudeTestUUID, subtype, "provider failed"))
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, refusalProviderFailed) || !strings.Contains(interp.Refusal, "provider failed") {
				t.Fatalf("interpretation=%+v err=%v", interp, err)
			}
		})
	}
	cases := []struct {
		name string
		line []byte
		want string
	}{
		{name: "missing result", line: []byte(initLine(claudeTestUUID)), want: refusalIncomplete},
		{name: "assistant only", line: []byte(initLine(claudeTestUUID) + "\n" + `{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}`), want: refusalIncomplete},
		{name: "empty success", line: jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, " \n\t")), want: refusalEmpty},
		{name: "max tokens", line: jsonl(initLine(claudeTestUUID), strings.Replace(resultLine(claudeTestUUID, "partial"), `"end_turn"`, `"max_tokens"`, 1)), want: refusalIncomplete},
		{name: "refusal", line: jsonl(initLine(claudeTestUUID), strings.Replace(resultLine(claudeTestUUID, "declined"), `"end_turn"`, `"refusal"`, 1)), want: refusalRefused},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stdout := testCase.line
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != testCase.want || answer.Len() != 0 {
				t.Fatalf("interpretation=%+v err=%v", interp, err)
			}
		})
	}
	successWithDiagnostic := jsonl(initLine(claudeTestUUID), `{"type":"result","subtype":"success","is_error":false,"result":"answer","errors":["unexpected failure"],"session_id":"`+claudeTestUUID+`"}`)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(successWithDiagnostic, claudeTestUUID), &claudeTestEvidence{stdout: successWithDiagnostic}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, refusalProviderFailed) || answer.Len() != 0 {
		t.Fatalf("success diagnostic interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestResultRequiresExactBooleanAndRejectsAnyErrorEntry(t *testing.T) {
	success := resultLine(claudeTestUUID, "answer")
	cases := []struct {
		name   string
		result string
		want   string
	}{
		{name: "null is_error", result: strings.Replace(success, `"is_error":false`, `"is_error":null`, 1), want: refusalMalformed},
		{name: "null errors", result: strings.Replace(success, `,"session_id":`, `,"errors":null,"session_id":`, 1), want: refusalMalformed},
		{name: "null diagnostic", result: resultLineWithErrors(success, `null`), want: refusalMalformed},
		{name: "trailing null diagnostic", result: resultLineWithErrors(success, `"error",null`), want: refusalMalformed},
		{name: "empty diagnostic", result: resultLineWithErrors(success, `""`), want: refusalProviderFailed},
		{name: "whitespace diagnostic", result: resultLineWithErrors(success, `" "`), want: refusalProviderFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stdout := jsonl(initLine(claudeTestUUID), testCase.result)
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
			if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, testCase.want) {
				t.Fatalf("interpretation=%+v want refusal prefix=%q err=%v", interp, testCase.want, err)
			}
		})
	}
	emptyErrors := strings.Replace(success, `,"session_id":`, `,"errors":[],"session_id":`, 1)
	stdout := jsonl(initLine(claudeTestUUID), emptyErrors)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "answer" {
		t.Fatalf("empty errors interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestModelNamesRejectUsageInvalidWhitespace(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{
			name: "init model",
			stdout: jsonl(
				strings.Replace(initLine(claudeTestUUID), `"model":"claude-test"`, `"model":" claude-test "`, 1),
				resultLine(claudeTestUUID, "answer"),
			),
		},
		{
			name: "modelUsage key",
			stdout: jsonl(
				initLine(claudeTestUUID),
				strings.Replace(resultLine(claudeTestUUID, "answer"), `"modelUsage":{"claude-test":`, `"modelUsage":{" claude-test ":`, 1),
			),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(testCase.stdout, claudeTestUUID), &claudeTestEvidence{stdout: testCase.stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed || answer.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
}

func TestInterruptionAndUnknownStopReasonsCannotPublish(t *testing.T) {
	for _, subtype := range []string{"interrupted", "error", "cancel", "timeout", "timed_out", "refusal"} {
		t.Run("system-"+subtype, func(t *testing.T) {
			stdout := jsonl(initLine(claudeTestUUID), fmt.Sprintf(`{"type":"system","subtype":%q}`, subtype), resultLine(claudeTestUUID, "answer"))
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
				t.Fatalf("interpretation=%+v err=%v", interp, err)
			}
		})
	}
	for _, subtype := range []string{"timeout", "timed_out", "refusal"} {
		t.Run("system-after-result-"+subtype, func(t *testing.T) {
			stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer"), fmt.Sprintf(`{"type":"system","subtype":%q}`, subtype))
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
				t.Fatalf("interpretation=%+v err=%v", interp, err)
			}
		})
	}
	for _, testCase := range []struct {
		name       string
		stopReason string
		want       string
	}{
		{name: "interrupted stop", stopReason: "interrupted", want: refusalProviderFailed},
		{name: "unknown stop", stopReason: "unknown", want: refusalMalformed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := strings.Replace(resultLine(claudeTestUUID, "answer"), `"end_turn"`, fmt.Sprintf("%q", testCase.stopReason), 1)
			stdout := jsonl(initLine(claudeTestUUID), result)
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != testCase.want {
				t.Fatalf("interpretation=%+v want=%q err=%v", interp, testCase.want, err)
			}
		})
	}
}

func TestSubstantiveUserEventAfterResultCannotPublish(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer"), fmt.Sprintf(`{"type":"user","session_id":%q,"message":{"content":[]}}`, claudeTestUUID))
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
}

func TestNativeToolUseNamesAreProviderOwned(t *testing.T) {
	stdout := jsonl(
		initLine(claudeTestUUID),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"tool_use","id":"blocked","name":"Bash","input":{"command":"true"}}]}}`, claudeTestUUID),
		resultLine(claudeTestUUID, "answer"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "answer" {
		t.Fatalf("native tool interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestLegacyPortableInterpreterRetainsToolRestriction(t *testing.T) {
	stdout := jsonl(
		initLine(claudeTestUUID),
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"tool_use","id":"blocked","name":"Bash","input":{"command":"true"}}]}}`, claudeTestUUID),
		resultLine(claudeTestUUID, "answer"),
	)
	var answer bytes.Buffer
	seal := claudeTestSealForPredicate(stdout, legacyPortableReferenceForMode(Mode))
	interp, err := newLegacyPortableInterpreter(Mode).Evaluate(predicate.Input{Seal: seal, ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed || answer.Len() != 0 {
		t.Fatalf("legacy tool restriction changed: interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestPreInitMessageAndConflictingIdentityCannotPublish(t *testing.T) {
	preInitSession := "123e4567-e89b-12d3-a456-426614174001"
	stdout := jsonl(
		fmt.Sprintf(`{"type":"user","session_id":%q,"message":{"content":[{"type":"text","text":"before init"}]}}`, preInitSession),
		initLine(claudeTestUUID),
		resultLine(claudeTestUUID, "answer"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed || answer.Len() != 0 {
		t.Fatalf("pre-init message interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestPermissionDenialsRequireObjectEntries(t *testing.T) {
	for _, entry := range []string{"null", "1", `"denied"`} {
		t.Run(entry, func(t *testing.T) {
			result := strings.Replace(resultLine(claudeTestUUID, "answer"), `,"session_id":`, `,"permission_denials":[`+entry+`],"session_id":`, 1)
			stdout := jsonl(initLine(claudeTestUUID), result)
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed || answer.Len() != 0 {
				t.Fatalf("permission denial interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
	nullField := strings.Replace(resultLine(claudeTestUUID, "answer"), `,"session_id":`, `,"permission_denials":null,"session_id":`, 1)
	stdout := jsonl(initLine(claudeTestUUID), nullField)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed || answer.Len() != 0 {
		t.Fatalf("null permission denials interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestDuplicateMalformedOversizedAndConflictingResultsReject(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{name: "duplicate result", stdout: jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "first"), resultLine(claudeTestUUID, "second"))},
		{name: "conflicting session", stdout: jsonl(initLine(claudeTestUUID), resultLine("123e4567-e89b-12d3-a456-426614174001", "answer"))},
		{name: "duplicate key", stdout: jsonl(initLine(claudeTestUUID), `{"type":"result","subtype":"success","is_error":false,"is_error":false,"result":"answer","session_id":"`+claudeTestUUID+`"}`)},
		{name: "truncated", stdout: append(jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer")), []byte("\n{\"type\":\"system\"")...)},
		{name: "invalid utf8", stdout: append(jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer")), []byte("\n\xff")...)},
		{name: "oversized", stdout: append(jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer")), append([]byte("\n{"), bytes.Repeat([]byte{'x'}, task.MaxControlRecordSize)...)...)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(claudeContinuationInput(testCase.stdout, claudeTestUUID), &claudeTestEvidence{stdout: testCase.stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || answer.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
}

func TestSealFailuresTakePrecedenceAfterEvidenceDrain(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer"))
	for _, alter := range []func(*task.ProviderExitRecord){
		func(seal *task.ProviderExitRecord) { seal.ExitCode = 7 },
		func(seal *task.ProviderExitRecord) { seal.Error = "capture failed" },
		func(seal *task.ProviderExitRecord) {
			seal.InvocationState = task.InvocationStartFailed
			seal.Error = "start failed"
		},
	} {
		seal := claudeTestSeal(stdout)
		alter(&seal)
		var answer bytes.Buffer
		interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: seal, ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}, &claudeTestEvidence{stdout: stdout, stderr: []byte("diagnostic")}, &answer)
		if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, refusalProviderFailed) || answer.Len() != 0 {
			t.Fatalf("interpretation=%+v err=%v", interp, err)
		}
	}
}

func TestIdentityMismatchRejectsWithoutWritingAnswer(t *testing.T) {
	fresh, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	stdout := jsonl(initLine(fresh), resultLine(fresh, "answer"))
	for _, input := range []predicate.Input{
		{Seal: claudeTestSeal(stdout), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}},
		{Seal: claudeTestSeal(stdout), ExpectedSession: task.SessionExpectation{}, RecordedSession: &task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}},
		{Seal: claudeTestSeal(stdout), ExpectedSession: task.SessionExpectation{}, RecordedSession: &task.SessionIdentity{Provider: "other:provider", ConversationID: fresh}},
	} {
		var answer bytes.Buffer
		interp, err := NewInterpreter().Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
		if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalIdentity || answer.Len() != 0 {
			t.Fatalf("interpretation=%+v err=%v", interp, err)
		}
	}
}

func TestReaderAndWriterFailuresRemainOperationalErrors(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, strings.Repeat("a", answerChunkBytes+10)))
	readErr := errors.New("stdout read failed")
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdoutReader: &claudeErrorReader{data: stdout, err: readErr}}, &bytes.Buffer{})
	if !errors.Is(err, readErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}

	writerErr := errors.New("answer write failed")
	var answer claudeFailingWriter
	answer.err = writerErr
	interp, err = NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if !errors.Is(err, writerErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
	short := &claudeShortWriter{remaining: 37}
	interp, err = NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, short)
	if !errors.Is(err, io.ErrShortWrite) || interp.Verdict != "" || short.Len() != 37 {
		t.Fatalf("short writer interpretation=%+v bytes=%d err=%v", interp, short.Len(), err)
	}
}

func TestSemanticFaultStillDrainsBothStreams(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), resultLine(claudeTestUUID, "answer"), `{"type":"turn.stopped"}`)
	stdoutReader := &claudeTrackingReader{data: stdout, chunk: 3}
	stderrReader := &claudeTrackingReader{data: []byte("diagnostic"), chunk: 2}
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdoutReader: stdoutReader, stderrReader: stderrReader}, &bytes.Buffer{})
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
	if stdoutReader.off != len(stdout) || !stdoutReader.eof || stderrReader.off != len("diagnostic") || !stderrReader.eof {
		t.Fatalf("streams not drained: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}
}

func TestUsagePreservesNullsAndRejectsNegativeCounters(t *testing.T) {
	stdout := jsonl(initLine(claudeTestUUID), `{"type":"result","subtype":"success","is_error":false,"result":"answer","session_id":"`+claudeTestUUID+`","usage":{"input_tokens":null,"output_tokens":2,"cache_read_input_tokens":null},"modelUsage":{"claude-test":{"inputTokens":null,"outputTokens":2,"costUSD":null}}}`)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(claudeContinuationInput(stdout, claudeTestUUID), &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || len(interp.Usage) != 2 {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
	if interp.Usage[0].InputTokens != nil || interp.Usage[0].CacheReadTokens != nil || interp.Usage[0].TotalTokens != nil {
		t.Fatalf("null usage became available: %+v", interp.Usage[0])
	}
	assertClaudeInt64(t, "output", interp.Usage[0].OutputTokens, 2)
	negative := jsonl(initLine(claudeTestUUID), `{"type":"result","subtype":"success","is_error":false,"result":"answer","session_id":"`+claudeTestUUID+`","usage":{"output_tokens":-1}}`)
	interp, err = NewInterpreter().Evaluate(claudeContinuationInput(negative, claudeTestUUID), &claudeTestEvidence{stdout: negative}, &bytes.Buffer{})
	if err != nil || interp.Verdict != task.VerdictRejected {
		t.Fatalf("negative usage interpretation=%+v err=%v", interp, err)
	}
}

func TestManyLargeAssistantEventsDoNotBecomeParserState(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(initLine(claudeTestUUID))
	large := strings.Repeat("draft", 1500)
	for index := 0; index < 256; index++ {
		if _, err := fmt.Fprintf(&stream, "\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]},\"session_id\":%q}", large, claudeTestUUID); err != nil {
			t.Fatal(err)
		}
	}
	stream.WriteByte('\n')
	stream.WriteString(resultLine(claudeTestUUID, "final"))
	state, err := parseStdout(strings.NewReader(stream.String()))
	if err != nil || state.semanticFault() || !state.resultSeen || state.result.answer != "final" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if len(state.result.answer) != len("final") || len(state.init.tools) != 3 {
		t.Fatalf("parser retained unexpected answer/init state: %+v", state)
	}
}

func resultLine(sessionID, answer string) string {
	encoded, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":%s,"stop_reason":"end_turn","session_id":%q,"usage":{"input_tokens":12,"output_tokens":8,"cache_read_input_tokens":3,"cache_creation_input_tokens":2},"modelUsage":{"claude-test":{"inputTokens":12,"outputTokens":8,"costUSD":0.0012}},"total_cost_usd":0.0012,"duration_ms":1500,"num_turns":1}`, encoded, sessionID)
}

func errorResultLine(sessionID, subtype, diagnostic string) string {
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"type":"result","subtype":%q,"is_error":true,"session_id":%q,"errors":[%s]}`, subtype, sessionID, encoded)
}

func resultLineWithErrors(result, entry string) string {
	return strings.Replace(result, `,"session_id":`, `,"errors":[`+entry+`],"session_id":`, 1)
}

func claudeInput(stdout []byte) predicate.Input {
	return predicate.Input{Seal: claudeTestSeal(stdout), ExpectedSession: task.SessionExpectation{}}
}

func claudeContinuationInput(stdout []byte, sessionID string) predicate.Input {
	return predicate.Input{Seal: claudeTestSeal(stdout), ExpectedSession: task.SessionExpectation{Required: true, ID: sessionID}}
}

func claudeTestSeal(stdout []byte) task.ProviderExitRecord {
	return claudeTestSealForPredicate(stdout, Reference())
}

func claudeTestSealForPredicate(stdout []byte, ref task.PredicateRef) task.ProviderExitRecord {
	manifest := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: task.ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256(stdout)},
	}
	data, err := task.MarshalCanonical(manifest)
	if err != nil {
		panic(err)
	}
	return task.ProviderExitRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        claudeTestRootID, TaskID: claudeTestTaskID,
		SpecSHA256: claudeTestSpecHash, MetaSHA256: claudeTestMetaHash,
		InvocationState: task.InvocationStarted, ExitCode: 0,
		Predicate: ref, RawManifest: manifest,
		ManifestSHA256: task.ComputeSHA256(data), ClosedAt: "2026-09-13T00:00:00Z",
	}
}

func assertClaudeInt64(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s=%v want=%d", name, got, want)
	}
}

type claudeTestEvidence struct {
	stdout       []byte
	stderr       []byte
	stdoutReader io.Reader
	stderrReader io.Reader
}

func (e *claudeTestEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	var reader io.Reader
	switch stream {
	case predicate.Stdout:
		reader = e.stdoutReader
		if reader == nil {
			reader = bytes.NewReader(e.stdout)
		}
	case predicate.Stderr:
		reader = e.stderrReader
		if reader == nil {
			reader = bytes.NewReader(e.stderr)
		}
	default:
		return fmt.Errorf("unknown stream %v", stream)
	}
	return consume(reader)
}

type claudeErrorReader struct {
	data []byte
	err  error
	off  int
}

func (r *claudeErrorReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

type claudeTrackingReader struct {
	data  []byte
	chunk int
	off   int
	eof   bool
}

func (r *claudeTrackingReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		r.eof = true
		return 0, io.EOF
	}
	n := r.chunk
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	if remaining := len(r.data) - r.off; n > remaining {
		n = remaining
	}
	copy(p[:n], r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

type claudeFailingWriter struct{ err error }

func (w *claudeFailingWriter) Write([]byte) (int, error) { return 0, w.err }

type claudeShortWriter struct {
	bytes.Buffer
	remaining int
}

func (w *claudeShortWriter) Write(data []byte) (int, error) {
	if w.remaining == 0 {
		return 0, nil
	}
	n := w.remaining
	if n > len(data) {
		n = len(data)
	}
	if _, err := w.Buffer.Write(data[:n]); err != nil {
		return 0, err
	}
	w.remaining -= n
	return n, nil
}
