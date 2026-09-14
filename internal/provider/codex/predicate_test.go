package codex

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	//go:embed testdata/success.jsonl
	fixtureSuccess []byte
	//go:embed testdata/retry-success.jsonl
	fixtureRetrySuccess []byte
	//go:embed testdata/turn-failed.jsonl
	fixtureTurnFailed []byte
	//go:embed testdata/missing-terminal.jsonl
	fixtureMissingTerminal []byte
	//go:embed testdata/blank-final.jsonl
	fixtureBlankFinal []byte
)

const (
	testRootID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSpecHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testMetaHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testThreadID = "123e4567-e89b-12d3-a456-426614174000"
)

func TestReferenceBindsContractAndThreadShape(t *testing.T) {
	ref := Reference()
	if ref.Adapter != Provider || ref.Mode != Mode || ref.Version != predicateVersion || ref.SHA256 != ContractDigest() {
		t.Fatalf("reference=%+v", ref)
	}
	if !strings.HasSuffix(Contract(), "\n") || task.ComputeSHA256([]byte(Contract())) != ref.SHA256 {
		t.Fatal("contract digest does not cover canonical bytes")
	}
	for _, id := range []string{testThreadID, strings.ToUpper(testThreadID)} {
		if !validThreadID(id) {
			t.Fatalf("valid UUID rejected: %q", id)
		}
	}
	for _, id := range []string{"", "not-a-uuid", "123e4567-e89b-12d3-a456-42661417400z", "123e4567e89b12d3a456426614174000"} {
		if validThreadID(id) {
			t.Fatalf("invalid UUID accepted: %q", id)
		}
	}
}

func TestCheckedInFixturesUsePinnedEventSemantics(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		stdout      []byte
		wantVerdict string
	}{
		{name: "success", stdout: fixtureSuccess, wantVerdict: task.VerdictCommitted},
		{name: "retry success", stdout: fixtureRetrySuccess, wantVerdict: task.VerdictCommitted},
		{name: "turn failed", stdout: fixtureTurnFailed, wantVerdict: task.VerdictRejected},
		{name: "missing terminal", stdout: fixtureMissingTerminal, wantVerdict: task.VerdictRejected},
		{name: "blank final", stdout: fixtureBlankFinal, wantVerdict: task.VerdictRejected},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(testCase.stdout, nil)}, &testEvidence{stdout: testCase.stdout}, &answer)
			if err != nil || interp.Verdict != testCase.wantVerdict {
				t.Fatalf("fixture interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
			if testCase.wantVerdict == task.VerdictRejected && answer.Len() != 0 {
				t.Fatalf("rejected fixture wrote answer=%q", answer.String())
			}
		})
	}
}

func TestSuccessSelectsLastTopLevelAgentMessageAndPreservesBytes(t *testing.T) {
	first := itemCompleted("commentary", itemAgentMessage, "thinking aloud")
	reasoning := itemCompleted("reasoning", "reasoning", "do not publish this")
	collab := itemCompleted("collab", "collab_tool_call", "nested text is not an answer")
	final := itemCompleted("final", itemAgentMessage, " final\nanswer ☃ ")
	stdout := jsonl(
		threadStarted(),
		`{"type":"turn.started"}`,
		first,
		reasoning,
		collab,
		final,
		turnCompleted(`{"input_tokens":12,"cached_input_tokens":3,"cache_write_input_tokens":2,"output_tokens":8,"reasoning_output_tokens":5}`),
	)
	seal := validSeal(stdout, nil)
	evidence := &testEvidence{stdout: stdout, stderr: []byte("diagnostic\n")}
	var answer bytes.Buffer
	interpretation, err := NewInterpreter().Evaluate(predicate.Input{Seal: seal}, evidence, &answer)
	if err != nil {
		t.Fatal(err)
	}
	if interpretation.Verdict != task.VerdictCommitted || interpretation.Refusal != "" || answer.String() != " final\nanswer ☃ " {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, answer.String())
	}
	wantSession := task.SessionIdentity{Provider: Provider, ConversationID: testThreadID}
	if interpretation.Session == nil || *interpretation.Session != wantSession {
		t.Fatalf("session=%+v", interpretation.Session)
	}
	if len(interpretation.Usage) != 1 {
		t.Fatalf("usage=%+v", interpretation.Usage)
	}
	usage := interpretation.Usage[0]
	if usage.Scope != task.UsageScopeConversationCumulative || usage.Source != task.UsageSourceProviderEvent || usage.Reliability != task.UsageReliabilityReported {
		t.Fatalf("usage identity=%+v", usage)
	}
	assertInt64(t, "input", usage.InputTokens, 12)
	assertInt64(t, "cache read", usage.CacheReadTokens, 3)
	assertInt64(t, "cache write", usage.CacheCreationTokens, 2)
	assertInt64(t, "output", usage.OutputTokens, 8)
	assertInt64(t, "reasoning", usage.ThinkingTokens, 5)
	if usage.TotalTokens != nil {
		t.Fatalf("provider did not report a total but one was synthesized: %+v", usage)
	}
}

func TestEarlierErrorMayRecoverBeforeSuccessfulCompletion(t *testing.T) {
	stdout := jsonl(
		threadStarted(),
		`{"type":"error","message":"transient connection retry"}`,
		`{"type":"turn.started"}`,
		itemCompleted("answer", itemAgentMessage, "recovered"),
		turnCompleted(`{"input_tokens":1}`),
	)
	var answer bytes.Buffer
	interpretation, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || answer.String() != "recovered" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, answer.String(), err)
	}
}

func TestUnknownTelemetryIsToleratedButUnknownLifecycleRejects(t *testing.T) {
	accepted := jsonl(
		threadStarted(),
		`{"type":"telemetry.snapshot","payload":{"attempt":1}}`,
		`{"type":"item.future","payload":"ignored"}`,
		`{"type":"turn.started"}`,
		itemCompleted("answer", itemAgentMessage, "accepted"),
		turnCompleted(`{}`),
	)
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(accepted, nil)}, &testEvidence{stdout: accepted}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "accepted" {
		t.Fatalf("telemetry interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}

	for _, lifecycle := range []string{"turn.stopped", "thread.finished"} {
		t.Run(lifecycle, func(t *testing.T) {
			stdout := jsonl(
				threadStarted(),
				`{"type":"turn.started"}`,
				itemCompleted("answer", itemAgentMessage, "ambiguous"),
				turnCompleted(`{}`),
				fmt.Sprintf(`{"type":%q}`, lifecycle),
			)
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, &answer)
			if err != nil || interp.Verdict != task.VerdictRejected || answer.Len() != 0 {
				t.Fatalf("lifecycle interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
			}
		})
	}
}

func TestItemStateIsBoundedAndStartedTextIsDiscarded(t *testing.T) {
	large := strings.Repeat("draft", 32*1024)
	started := fmt.Sprintf(`{"type":"item.started","item":{"id":"answer","type":"agent_message","text":%q}}`, large)
	updated := fmt.Sprintf(`{"type":"item.updated","item":{"id":"answer","type":"agent_message","text":%q}}`, large)
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, started, updated, itemCompleted("answer", itemAgentMessage, "final"), turnCompleted(`{}`))
	state, err := parseStdout(bytes.NewReader(stdout))
	if err != nil || state.semantic != nil || !state.finalSeen || string(state.finalMessage) != "final" {
		t.Fatalf("large item state=%+v err=%v", state, err)
	}
	if len(state.items) != 1 {
		t.Fatalf("item state count=%d want=1", len(state.items))
	}

	var many strings.Builder
	many.WriteString(threadStarted())
	many.WriteString("\n{\"type\":\"turn.started\"}")
	for index := 0; index <= maxTrackedItems; index++ {
		if _, writeErr := fmt.Fprintf(&many, "\n{\"type\":\"item.completed\",\"item\":{\"id\":\"item-%d\",\"type\":\"tool_call\"}}", index); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	many.WriteString("\n")
	many.WriteString(turnCompleted(`{}`))
	state, err = parseStdout(bytes.NewReader([]byte(many.String())))
	if err != nil {
		t.Fatal(err)
	}
	if state.semantic == nil || len(state.items) != maxTrackedItems {
		t.Fatalf("bounded item state semantic=%v count=%d want=%d", state.semantic, len(state.items), maxTrackedItems)
	}
}

func TestSemanticFailureStopsLaterStateAccumulation(t *testing.T) {
	stdout := jsonl(
		threadStarted(),
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"bad","text":"duplicate"}}`,
		itemCompleted("later", itemAgentMessage, "must not become candidate"),
		turnCompleted(`{}`),
	)
	state, err := parseStdout(bytes.NewReader(stdout))
	if err != nil || state.semantic == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if state.finalSeen || len(state.items) != 0 {
		t.Fatalf("state accumulated after semantic failure: final=%v items=%d", state.finalSeen, len(state.items))
	}
}

func TestItemCompletionUpdatesStartedCandidateByID(t *testing.T) {
	started := fmt.Sprintf(`{"type":"item.started","item":{"id":"answer","type":"agent_message","text":%q}}`, "draft")
	updated := fmt.Sprintf(`{"type":"item.updated","item":{"id":"answer","type":"agent_message","text":%q}}`, "updated")
	completed := itemCompleted("answer", itemAgentMessage, "final")
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, started, updated, completed, turnCompleted(`{}`))
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || answer.String() != "final" {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interp, answer.String(), err)
	}
}

func TestOutputArtifactMustAgreeByteForByteAndAbsenceFallsBack(t *testing.T) {
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "exact"), turnCompleted(`{}`))
	for _, testCase := range []struct {
		name        string
		artifact    *[]byte
		wantVerdict string
		wantAnswer  string
	}{
		{name: "absent", wantVerdict: task.VerdictCommitted, wantAnswer: "exact"},
		{name: "matching", artifact: bytesPtr([]byte("exact")), wantVerdict: task.VerdictCommitted, wantAnswer: "exact"},
		{name: "empty", artifact: bytesPtr(nil), wantVerdict: task.VerdictRejected},
		{name: "conflict", artifact: bytesPtr([]byte("different")), wantVerdict: task.VerdictRejected},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, testCase.artifact)}, &testEvidence{stdout: stdout, artifact: testCase.artifact}, &answer)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != testCase.wantVerdict {
				t.Fatalf("interpretation=%+v", interp)
			}
			if testCase.wantVerdict == task.VerdictCommitted && answer.String() != testCase.wantAnswer {
				t.Fatalf("answer=%q", answer.String())
			}
			if testCase.wantVerdict == task.VerdictRejected && answer.Len() != 0 {
				t.Fatalf("rejected answer sink received %q", answer.String())
			}
		})
	}
}

func TestMalformedAndTerminalEvidenceRejects(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{name: "missing terminal", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"))},
		{name: "blank final", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, " \n\t"), turnCompleted(`{}`))},
		{name: "turn failed", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, `{"type":"turn.failed","error":{"message":"failed"}}`)},
		{name: "interrupted", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, `{"type":"turn.interrupted"}`)},
		{name: "multiple IDs", stdout: jsonl(threadStarted(), `{"type":"thread.started","thread_id":"123e4567-e89b-12d3-a456-426614174001"}`, `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`))},
		{name: "duplicate completion", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`), turnCompleted(`{}`))},
		{name: "duplicate keys", stdout: jsonl(threadStarted(), `{"type":"turn.started"}`, `{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"answer","text":"other"}}`, turnCompleted(`{}`))},
		{name: "truncated final record", stdout: append(jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`)), []byte(`\n{"type":"item.completed"`)...)},
		{name: "invalid UTF-8", stdout: append(jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`)), []byte("\n\xff")...)},
		{name: "oversized line", stdout: append(jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`)), append([]byte("\n{"), bytes.Repeat([]byte("x"), task.MaxControlRecordSize)...)...)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var answer bytes.Buffer
			interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(testCase.stdout, nil)}, &testEvidence{stdout: testCase.stdout}, &answer)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != task.VerdictRejected || answer.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q", interp, answer.String())
			}
		})
	}
}

func TestExpectedAndRecordedIdentityMustMatch(t *testing.T) {
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{}`))
	wrong := "123e4567-e89b-12d3-a456-426614174001"
	for _, input := range []predicate.Input{
		{Seal: validSeal(stdout, nil), ExpectedSession: task.SessionExpectation{Required: true, ID: wrong}},
		{Seal: validSeal(stdout, nil), RecordedSession: &task.SessionIdentity{Provider: Provider, ConversationID: wrong}},
		{Seal: validSeal(stdout, nil), RecordedSession: &task.SessionIdentity{Provider: "other:provider", ConversationID: testThreadID}},
	} {
		var answer bytes.Buffer
		interp, err := NewInterpreter().Evaluate(input, &testEvidence{stdout: stdout}, &answer)
		if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalIdentity {
			t.Fatalf("interpretation=%+v err=%v", interp, err)
		}
	}
}

func TestSealFailureAndNonzeroExitRejectAfterEvidenceDrain(t *testing.T) {
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{"output_tokens":3}`))
	for _, alter := range []func(*task.ProviderExitRecord){
		func(seal *task.ProviderExitRecord) { seal.ExitCode = 7 },
		func(seal *task.ProviderExitRecord) { seal.Error = "capture failed" },
	} {
		var answer bytes.Buffer
		interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: alteredSeal(stdout, nil, alter)}, &testEvidence{stdout: stdout, stderr: []byte("stderr")}, &answer)
		if err != nil || interp.Verdict != task.VerdictRejected || !strings.HasPrefix(interp.Refusal, refusalProviderFailed) || answer.Len() != 0 {
			t.Fatalf("interpretation=%+v err=%v", interp, err)
		}
	}
}

func TestReaderAndWriterFailuresRemainOperationalErrors(t *testing.T) {
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, strings.Repeat("a", answerChunkBytes+10)), turnCompleted(`{}`))
	readErr := errors.New("stdout read failed")
	evidence := &testEvidence{stdoutReader: &errorReader{data: stdout, err: readErr}, stderr: []byte("drain")}
	interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, evidence, &bytes.Buffer{})
	if !errors.Is(err, readErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}

	writerErr := errors.New("answer write failed")
	var answer failingWriter
	answer.err = writerErr
	interp, err = NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, &answer)
	if !errors.Is(err, writerErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}

	short := &shortWriter{remaining: 37}
	interp, err = NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, short)
	if !errors.Is(err, io.ErrShortWrite) || interp.Verdict != "" || short.Len() != 37 {
		t.Fatalf("short writer interpretation=%+v bytes=%d err=%v", interp, short.Len(), err)
	}
}

func TestSemanticAndWriterFaultsStillDrainBothStreams(t *testing.T) {
	semanticStdout := jsonl(
		threadStarted(),
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"answer","text":"conflict"}}`,
		itemCompleted("later", itemAgentMessage, "later"),
		turnCompleted(`{}`),
	)
	stdoutReader := &trackingEOFReader{data: semanticStdout, chunk: 3}
	stderrReader := &trackingEOFReader{data: []byte("diagnostic"), chunk: 2}
	interp, err := NewInterpreter().Evaluate(
		predicate.Input{Seal: validSeal(semanticStdout, nil)},
		&testEvidence{stdoutReader: stdoutReader, stderrReader: stderrReader},
		&bytes.Buffer{},
	)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalMalformed {
		t.Fatalf("semantic interpretation=%+v err=%v", interp, err)
	}
	if stdoutReader.off != len(semanticStdout) || !stdoutReader.eof || stderrReader.off != len("diagnostic") || !stderrReader.eof {
		t.Fatalf("semantic fault did not drain streams: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}

	answerText := strings.Repeat("answer", answerChunkBytes)
	writerStdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, answerText), turnCompleted(`{}`))
	writerErr := errors.New("answer writer failed")
	stdoutReader = &trackingEOFReader{data: writerStdout, chunk: 5}
	stderrReader = &trackingEOFReader{data: []byte("diagnostic"), chunk: 2}
	writer := &boundedFailWriter{limit: 100, err: writerErr}
	interp, err = NewInterpreter().Evaluate(
		predicate.Input{Seal: validSeal(writerStdout, nil)},
		&testEvidence{stdoutReader: stdoutReader, stderrReader: stderrReader},
		writer,
	)
	if !errors.Is(err, writerErr) || interp.Verdict != "" {
		t.Fatalf("writer interpretation=%+v err=%v", interp, err)
	}
	if stdoutReader.off != len(writerStdout) || !stdoutReader.eof || stderrReader.off != len("diagnostic") || !stderrReader.eof {
		t.Fatalf("writer fault did not drain streams: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}
}

func TestMissingUsageCountersStayNull(t *testing.T) {
	stdout := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{"input_tokens":null,"output_tokens":2,"reasoning_output_tokens":null}`))
	var answer bytes.Buffer
	interp, err := NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(stdout, nil)}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || interp.Verdict != task.VerdictCommitted || len(interp.Usage) != 1 {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
	usage := interp.Usage[0]
	if usage.InputTokens != nil || usage.ThinkingTokens != nil || usage.CacheReadTokens != nil || usage.CacheCreationTokens != nil || usage.TotalTokens != nil {
		t.Fatalf("missing counters became available: %+v", usage)
	}
	assertInt64(t, "output", usage.OutputTokens, 2)

	negative := jsonl(threadStarted(), `{"type":"turn.started"}`, itemCompleted("answer", itemAgentMessage, "answer"), turnCompleted(`{"output_tokens":-1}`))
	interp, err = NewInterpreter().Evaluate(predicate.Input{Seal: validSeal(negative, nil)}, &testEvidence{stdout: negative}, &bytes.Buffer{})
	if err != nil || interp.Verdict != task.VerdictRejected {
		t.Fatalf("negative usage interpretation=%+v err=%v", interp, err)
	}
}

func threadStarted() string {
	return `{"type":"thread.started","thread_id":"` + testThreadID + `"}`
}

func itemCompleted(id, typeName, text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"type":"item.completed","item":{"id":%q,"type":%q,"text":%s}}`, id, typeName, encoded)
}

func turnCompleted(usage string) string {
	return `{"type":"turn.completed","usage":` + usage + `}`
}

func jsonl(lines ...string) []byte { return []byte(strings.Join(lines, "\n")) }

func validSeal(stdout []byte, artifact *[]byte) task.ProviderExitRecord {
	return alteredSeal(stdout, artifact, nil)
}

func alteredSeal(stdout []byte, artifact *[]byte, alter func(*task.ProviderExitRecord)) task.ProviderExitRecord {
	manifest := []task.RawManifestEntry{
		{Path: "raw/" + OutputName, Size: int64(lenOrZero(artifact)), SHA256: digestOrNil(artifact)},
		{Path: "raw/stderr", Size: 0, SHA256: task.ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256(stdout)},
	}
	if artifact == nil {
		manifest = manifest[1:]
	}
	data, err := task.MarshalCanonical(manifest)
	if err != nil {
		panic(err)
	}
	seal := task.ProviderExitRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          testRootID,
		TaskID:          testTaskID,
		SpecSHA256:      testSpecHash,
		MetaSHA256:      testMetaHash,
		InvocationState: task.InvocationStarted,
		ExitCode:        0,
		Predicate:       Reference(),
		RawManifest:     manifest,
		ManifestSHA256:  task.ComputeSHA256(data),
		ClosedAt:        "2026-09-13T00:00:00Z",
	}
	if alter != nil {
		alter(&seal)
	}
	return seal
}

func assertInt64(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s=%v want=%d", name, got, want)
	}
}

func bytesPtr(value []byte) *[]byte { return &value }

func lenOrZero(value *[]byte) int {
	if value == nil {
		return 0
	}
	return len(*value)
}

func digestOrNil(value *[]byte) string {
	if value == nil {
		return task.ComputeSHA256(nil)
	}
	return task.ComputeSHA256(*value)
}

type testEvidence struct {
	stdout       []byte
	stderr       []byte
	artifact     *[]byte
	stdoutReader io.Reader
	stderrReader io.Reader
	artifactErr  error
}

func (e *testEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
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

func (e *testEvidence) ReadNamed(name string, consume func(io.Reader) error) error {
	if name != OutputName {
		return task.ErrEvidenceFault
	}
	if e.artifactErr != nil {
		return e.artifactErr
	}
	if e.artifact == nil {
		return predicate.ErrEvidenceAbsent
	}
	return consume(bytes.NewReader(*e.artifact))
}

type errorReader struct {
	data []byte
	err  error
	off  int
}

type trackingEOFReader struct {
	data  []byte
	chunk int
	off   int
	eof   bool
}

func (r *trackingEOFReader) Read(p []byte) (int, error) {
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

type boundedFailWriter struct {
	limit int
	wrote int
	err   error
}

type shortWriter struct {
	bytes.Buffer
	remaining int
}

func (w *shortWriter) Write(data []byte) (int, error) {
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

func (w *boundedFailWriter) Write(data []byte) (int, error) {
	if w.wrote >= w.limit {
		return 0, w.err
	}
	n := w.limit - w.wrote
	if n > len(data) {
		n = len(data)
	}
	w.wrote += n
	if n < len(data) {
		return n, w.err
	}
	return n, nil
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }
