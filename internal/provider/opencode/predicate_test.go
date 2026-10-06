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

func TestCurrentOpenCodeInterpreterWarnsOnUnreliableUsage(t *testing.T) {
	stdout := jsonl(stepStart(testSession), textEvent(testSession, "answer"), stepFinish(testSession, "stop", `{"total":-1}`, "0.004"))
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "answer" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
	if len(interp.Diagnostics) != 1 || interp.Diagnostics[0].Code != "optional_usage_unavailable" || strings.Contains(interp.Diagnostics[0].Message, "tokens") {
		t.Fatalf("usage diagnostics=%+v", interp.Diagnostics)
	}
	if len(interp.Usage) != 1 || interp.Usage[0].Reliability != task.UsageReliabilityUnreliable || interp.Usage[0].TotalTokens != nil {
		t.Fatalf("unreliable usage=%+v", interp.Usage)
	}
}

func TestCurrentOpenCodeDiagnosticDoesNotExposeProviderText(t *testing.T) {
	secret := "provider-secret-value"
	stdout := jsonl(
		stepStart(testSession),
		textEvent(testSession, "answer"),
		fmt.Sprintf(`{"type":"error","sessionID":%q,"error":{"name":%q}}`, testSession, secret),
		stepFinish(testSession, "stop", `{}`, "0"),
	)
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, io.Discard)
	if err != nil || result.Verdict != task.VerdictRejected || len(result.Diagnostics) == 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Code, secret) || strings.Contains(diagnostic.Message, secret) {
			t.Fatalf("provider text leaked through diagnostic: %+v", diagnostic)
		}
	}
}

func TestCurrentInterpreterSkipsLargeOpaqueToolOutputAndDuplicateMetadata(t *testing.T) {
	opaque := strings.Repeat("x", task.MaxControlRecordSize+4096)
	tool := fmt.Sprintf(`{"type":"tool_use","sessionID":%q,"part":{"type":"tool","tool":"read","state":{"status":"completed","output":%q,"metadata":{"same":1,"same":2}}},"metadata":{"same":1,"same":2}}`, testSession, opaque)
	stdout := jsonl(stepStart(testSession), tool, textEvent(testSession, "small answer"), stepFinish(testSession, "stop", `{}`, "0"))
	var answer bytes.Buffer
	interpretation, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != "small answer" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, answer.String(), err)
	}
}

func TestCurrentInterpreterStreamsThirtyTwoMiBOpenCodeAnswer(t *testing.T) {
	answerText := strings.Repeat("a", (32<<20)+1)
	stdout := jsonl(stepStart(testSession), textEvent(testSession, answerText), stepFinish(testSession, "stop", `{}`, "0"))
	var sink opencodeCountingWriter
	interpretation, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &sink)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || sink.count != len(answerText) || sink.max > 64<<10 {
		t.Fatalf("interpretation=%+v count=%d max=%d err=%v", interpretation, sink.count, sink.max, err)
	}
}

func TestCurrentInterpreterStreamsFragmentedOpenCodeAnswer(t *testing.T) {
	const fragments = 2048
	lines := make([]string, 0, fragments+2)
	lines = append(lines, stepStart(testSession))
	for range fragments {
		lines = append(lines, textEvent(testSession, "x"))
	}
	lines = append(lines, stepFinish(testSession, "stop", `{}`, "0"))
	var answer bytes.Buffer
	interpretation, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(jsonl(lines...), ModeReadOnly)},
		&testEvidence{stdout: jsonl(lines...)}, &answer,
	)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != strings.Repeat("x", fragments) {
		t.Fatalf("interpretation=%+v answer length=%d err=%v", interpretation, answer.Len(), err)
	}
}

func TestCurrentInterpreterRecoversHistoricalWireWithoutChangingSeal(t *testing.T) {
	stdout := jsonl(stepStart(testSession), textEvent(testSession, "recovered"), stepFinish(testSession, "stop", `{}`, "0"))
	current := NewInterpreter(ModeReadOnly)
	recoverable, ok := current.(interface {
		Target() task.PredicateRef
		CanRecover(task.PredicateRef) bool
		Recover(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error)
	})
	if !ok || recoverable.CanRecover(current.Reference()) {
		t.Fatal("current OpenCode interpreter recovery mapping is incorrect")
	}
	for _, historical := range []predicate.Interpreter{
		newHistoricalCurrentInterpreter(ModeReadOnly),
		newHistoricalCurrentV3Interpreter(ModeReadOnly),
	} {
		t.Run(historical.Reference().Version, func(t *testing.T) {
			seal := validSeal(stdout, ModeReadOnly)
			seal.Predicate = historical.Reference()
			input := predicate.Input{Seal: seal}
			if !recoverable.CanRecover(historical.Reference()) {
				t.Fatal("current OpenCode interpreter does not recover historical predicate")
			}
			var answer bytes.Buffer
			interpretation, err := recoverable.Recover(input, &testEvidence{stdout: stdout}, &answer)
			if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != "recovered" || !input.Seal.Predicate.Equal(historical.Reference()) {
				t.Fatalf("recovery interpretation=%+v answer=%q err=%v seal=%+v", interpretation, answer.String(), err, input.Seal.Predicate)
			}
		})
	}
}

func TestCurrentInterpreterAllowsLaterStepsAfterIntermediateStop(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		toolEvent(testSession, "read"),
		stepFinish(testSession, "stop", `{}`, "0"),
		stepStart(testSession),
		textEvent(testSession, "final answer"),
		stepFinish(testSession, "stop", `{}`, "0"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "final answer" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}

	strictSeal := validSeal(stdout, ModeReadOnly)
	strictSeal.Predicate = historicalCurrentReferenceForMode(ModeReadOnly)
	answer.Reset()
	interp, err = newHistoricalCurrentV3Interpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: strictSeal}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictRejected || answer.Len() != 0 || interp.Refusal != refusalMalformed {
		t.Fatalf("historical interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestCurrentInterpreterRequiresTheLatestStepToStop(t *testing.T) {
	stdout := jsonl(
		stepStart(testSession),
		textEvent(testSession, "intermediate"),
		stepFinish(testSession, "stop", `{}`, "0"),
		stepStart(testSession),
		textEvent(testSession, "unfinished"),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(stdout, ModeReadOnly)}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalIncomplete || answer.Len() != 0 {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
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
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "final" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
	historicalSeal := validSeal(stdout, ModeReadOnly)
	historicalSeal.Predicate = historicalReferenceForMode(ModeReadOnly)
	answer.Reset()
	interp, err = newHistoricalCurrentInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: historicalSeal}, &testEvidence{stdout: stdout}, &answer,
	)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "first final" {
		t.Fatalf("historical interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
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

func (r *errorReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(r.data)) {
		return 0, r.err
	}
	n := copy(buffer, r.data[offset:])
	if n < len(buffer) {
		return n, r.err
	}
	return n, nil
}

type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

func assertInt(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s=%v want=%d", name, got, want)
	}
}

type opencodeCountingWriter struct{ count, max int }

func (w *opencodeCountingWriter) Write(data []byte) (int, error) {
	w.count += len(data)
	if len(data) > w.max {
		w.max = len(data)
	}
	return len(data), nil
}
