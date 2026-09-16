package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReferenceBindsModeSpecificContracts(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		ref := ReferenceForMode(mode)
		if ref.Adapter != Provider || ref.Mode != mode || ref.Version != predicateVersion || ref.SHA256 != task.ComputeSHA256([]byte(ContractForMode(mode))) {
			t.Fatalf("mode=%s ref=%+v", mode, ref)
		}
		if !strings.HasSuffix(ContractForMode(mode), "\n") {
			t.Fatalf("mode=%s contract has no trailing newline", mode)
		}
	}
	if ReferenceForMode("unknown") != (task.PredicateRef{}) {
		t.Fatal("unsupported mode returned a predicate reference")
	}
}

func TestOpenCodeSuccessParsesTextIdentityAndUsage(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		textEvent(testSession, "answer "),
		textEvent(testSession, "☃"),
		stepFinish(testSession, "stop", `{"total":12,"input":4,"output":3,"reasoning":1,"cache":{"read":2,"write":1}}`, "0.004"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeWorkspaceWrite).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeWorkspaceWrite)},
		&testEvidence{stdout: stdout, stderr: []byte("diagnostic")}, &answer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictCommitted || interp.Refusal != "" || answer.String() != "answer ☃" {
		t.Fatalf("interpretation=%+v answer=%q", interp, answer.String())
	}
	if interp.Session == nil || interp.Session.Provider != Provider || interp.Session.ConversationID != testSession {
		t.Fatalf("session=%+v", interp.Session)
	}
	if len(interp.Usage) != 1 {
		t.Fatalf("usage=%+v", interp.Usage)
	}
	usage := interp.Usage[0]
	if usage.Scope != task.UsageScopeUnknown || usage.Source != task.UsageSourceProviderEvent || usage.Reliability != task.UsageReliabilityReported {
		t.Fatalf("usage identity=%+v", usage)
	}
	assertInt(t, "input", usage.InputTokens, 4)
	assertInt(t, "output", usage.OutputTokens, 3)
	assertInt(t, "reasoning", usage.ThinkingTokens, 1)
	assertInt(t, "total", usage.TotalTokens, 12)
	assertInt(t, "cache read", usage.CacheReadTokens, 2)
	assertInt(t, "cache write", usage.CacheCreationTokens, 1)
	if usage.EstimatedCostUSD == nil || *usage.EstimatedCostUSD != "0.004" {
		t.Fatalf("cost=%+v", usage.EstimatedCostUSD)
	}
	if err := task.ValidateInterpretation(interp); err != nil {
		t.Fatalf("interpretation validation: %v", err)
	}
}

func TestOpenCodeAllowsToolStepsButUsesOnlyText(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		textEvent(testSession, "first "),
		toolEvent(testSession, "read"),
		stepFinish(testSession, "tool-calls", `{}`, "0"),
		stepStart(testSession),
		textEvent(testSession, "final"),
		stepFinish(testSession, "stop", `{}`, "0"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "first final" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestOpenCodeAllowsRecoverableToolErrors(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		`{"type":"tool_use","sessionID":"`+testSession+`","part":{"type":"tool","tool":"read","state":{"status":"error","error":"transient read failure"}}}`,
		stepFinish(testSession, "tool-calls", `{}`, "0"),
		stepStart(testSession),
		textEvent(testSession, "retried successfully"),
		stepFinish(testSession, "stop", `{}`, "0"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "retried successfully" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestOpenCodeRequiresNonWhitespaceTerminalText(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		textEvent(testSession, "intermediate text"),
		stepFinish(testSession, "tool-calls", `{}`, "0"),
		stepStart(testSession),
		textEvent(testSession, "  \n\t"),
		stepFinish(testSession, "stop", `{}`, "0"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalEmpty || answer.Len() != 0 {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestOpenCodeRejectsMalformedMissingTerminalAndConflictingIdentity(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{name: "missing terminal", stdout: jsonl(stepStart(testSession), textEvent(testSession, "answer"))},
		{name: "unknown event", stdout: jsonl(stepStart(testSession), `{"type":"new_event","sessionID":"`+testSession+`"}`)},
		{name: "duplicate keys", stdout: []byte(fmt.Sprintf(`{"type":"step_start","type":"text","sessionID":%q,"part":{"type":"step-start"}}`, testSession))},
		{name: "conflicting identity", stdout: jsonl(stepStart(testSession), textEvent("ses_other", "answer"))},
		{name: "event after terminal", stdout: jsonl(stepStart(testSession), textEvent(testSession, "answer"), stepFinish(testSession, "stop", `{}`, "0"), textEvent(testSession, "late"))},
		{name: "invalid UTF-8", stdout: append(jsonl(stepStart(testSession), textEvent(testSession, "answer")), []byte("\n\xff")...)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter(ModeReadOnly).Evaluate(
				predicate.Input{Seal: validSeal(testCase.stdout, ModeReadOnly)}, &testEvidence{stdout: testCase.stdout}, &answer,
			)
			if err != nil || interp.Verdict != task.VerdictRejected || answer.Len() != 0 || interp.Refusal != refusalMalformed && interp.Refusal != refusalIncomplete {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
}

func TestOpenCodeRejectsProviderAndIdentityFailures(t *testing.T) {
	stdout := jsonl(stepStart(testSession), textEvent(testSession, "answer"), stepFinish(testSession, "stop", `{}`, "0"))
	wrongSession := "ses_otherSession_123"
	for _, input := range []predicate.Input{
		{Seal: validSeal(stdout, ModeReadOnly), ExpectedSession: task.SessionExpectation{Required: true, ID: wrongSession}},
		{Seal: validSeal(stdout, ModeReadOnly), RecordedSession: &task.SessionIdentity{Provider: Provider, ConversationID: wrongSession}},
		{Seal: validSeal(stdout, ModeReadOnly), RecordedSession: &task.SessionIdentity{Provider: "other:provider", ConversationID: testSession}},
	} {
		var answer bytes.Buffer
		interp, err := NewInterpreter(ModeReadOnly).Evaluate(input, &testEvidence{stdout: stdout}, &answer)
		if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalIdentity || answer.Len() != 0 {
			t.Fatalf("identity interpretation=%+v err=%v", interp, err)
		}
	}

	failure := jsonl(stepStart(testSession), `{"type":"error","sessionID":"`+testSession+`","error":{"name":"APIError"}}`)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(failure, ModeReadOnly)}, &testEvidence{stdout: failure}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalProviderFailed {
		t.Fatalf("provider failure interpretation=%+v err=%v", interp, err)
	}

	nonzero := validSeal(stdout, ModeReadOnly)
	nonzero.ExitCode = 7
	interp, err = NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: nonzero}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, refusalProviderFailed) {
		t.Fatalf("nonzero interpretation=%+v err=%v", interp, err)
	}
}

func TestOpenCodeAnswerWriterFailuresRemainOperational(t *testing.T) {
	stdout := jsonl(stepStart(testSession), textEvent(testSession, strings.Repeat("a", answerChunkBytes+10)), stepFinish(testSession, "stop", `{}`, "0"))
	writeErr := errors.New("answer write failed")
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &failingWriter{err: writeErr})
	if !errors.Is(err, writeErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}

	readErr := errors.New("stdout read failed")
	interp, err = NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdoutReader: &errorReader{data: stdout, err: readErr}}, &bytes.Buffer{})
	if !errors.Is(err, readErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
}

func TestOpenCodeBoundsUsageRows(t *testing.T) {
	state := newEventState()
	usage := &usageTotals{present: true}
	for range maxUsageRows {
		appendUsage(&state, usage)
	}
	if state.semanticErr != nil || len(state.usage) != maxUsageRows {
		t.Fatalf("before bound state=%+v", state)
	}
	appendUsage(&state, usage)
	if state.semanticErr == nil || len(state.usage) != maxUsageRows {
		t.Fatalf("after bound state=%+v", state)
	}
}

func stepStart(session string) string {
	return fmt.Sprintf(`{"type":"step_start","sessionID":%q,"part":{"type":"step-start"}}`, session)
}

func textEvent(session, text string) string {
	return fmt.Sprintf(`{"type":"text","sessionID":%q,"part":{"type":"text","text":%q}}`, session, text)
}

func toolEvent(session, tool string) string {
	return fmt.Sprintf(`{"type":"tool_use","sessionID":%q,"part":{"type":"tool","tool":%q,"state":{"status":"completed"}}}`, session, tool)
}

func stepFinish(session, reason, tokens, cost string) string {
	return fmt.Sprintf(`{"type":"step_finish","sessionID":%q,"part":{"type":"step-finish","reason":%q,"tokens":%s,"cost":%s}}`, session, reason, tokens, cost)
}

func jsonl(lines ...string) []byte { return []byte(strings.Join(lines, "\n")) }

func validSeal(stdout []byte, mode string) task.ProviderExitRecord {
	manifest := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: task.ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256(stdout)},
	}
	data, err := task.MarshalCanonical(manifest)
	if err != nil {
		panic(err)
	}
	return task.ProviderExitRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          testRootID,
		TaskID:          testTaskID,
		SpecSHA256:      strings.Repeat("c", 64),
		MetaSHA256:      strings.Repeat("d", 64),
		InvocationState: task.InvocationStarted,
		ExitCode:        0,
		Predicate:       ReferenceForMode(mode),
		RawManifest:     manifest,
		ManifestSHA256:  task.ComputeSHA256(data),
		ClosedAt:        "2026-09-16T00:00:00Z",
	}
}

type testEvidence struct {
	stdout       []byte
	stderr       []byte
	stdoutReader io.Reader
	stderrReader io.Reader
}

func (e *testEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	reader := io.Reader(bytes.NewReader(e.stderr))
	if stream == predicate.Stdout {
		reader = bytes.NewReader(e.stdout)
		if e.stdoutReader != nil {
			reader = e.stdoutReader
		}
	} else if stream == predicate.Stderr && e.stderrReader != nil {
		reader = e.stderrReader
	}
	return consume(reader)
}

type errorReader struct {
	data []byte
	err  error
	off  int
}

func (r *errorReader) Read(buffer []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(buffer, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

func assertInt(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s=%v want=%d", name, got, want)
	}
}
