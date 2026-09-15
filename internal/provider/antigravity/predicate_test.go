package antigravity

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
	testRootID       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSpecSHA256   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testMetaSHA256   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testConversation = "123e4567-e89b-12d3-a456-426614174000"
)

func TestPrintReferenceAndContractDigest(t *testing.T) {
	ref := NewPrintInterpreter().Reference()
	want := task.PredicateRef{Adapter: Provider, Mode: Mode, Version: Version, SHA256: "9aca4a6a639c18b8c4515445d8f004f2788c0f627264695cbb66357b5285d366"}
	if ref != want {
		t.Fatalf("reference = %+v, want %+v", ref, want)
	}
	if ref.Adapter != "antigravity:print" || ref.Mode != "workspace-write" || ref.Version != "1.2.2" {
		t.Fatalf("reference lost immutable provider contract: %+v", ref)
	}
	if !strings.HasSuffix(Contract(), "\n") || task.ComputeSHA256([]byte(Contract())) != ref.SHA256 {
		t.Fatal("contract digest or terminator changed")
	}
}

func TestSuccessPayloadDecodesExactlyAndCopiesUsage(t *testing.T) {
	stdout := []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"  lead\nline\r\t☃ \"quoted\" \/ \\backslash \u0000 \ud83d\ude00  " ,"duration_seconds":1.250e+0,"num_turns":3,"usage":{"input_tokens":0,"output_tokens":25,"thinking_tokens":7,"cache_read_tokens":null,"total_tokens":32}}`)
	wantAnswer := "  lead\nline\r\t☃ \"quoted\" / \\backslash \x00 😀  "
	interp, answer, outReader, errReader, err := evaluateAntigravity(t, stdout, []byte("ordinary diagnostic\n"), nil, task.SessionExpectation{}, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertCommittedInterpretation(t, interp, answer, wantAnswer)
	assertReportedUsage(t, interp)
	assertReadersDrained(t, outReader, errReader, len(stdout), len("ordinary diagnostic\n"))
}

func assertCommittedInterpretation(t *testing.T, interp task.Interpretation, answer []byte, wantAnswer string) {
	t.Helper()
	if interp.Verdict != task.VerdictCommitted || interp.Refusal != "" || string(answer) != wantAnswer {
		t.Fatalf("interpretation=%+v answer=%q", interp, answer)
	}
	wantSession := task.SessionIdentity{Provider: Provider, ConversationID: testConversation}
	if interp.Session == nil || *interp.Session != wantSession {
		t.Fatalf("session=%+v", interp.Session)
	}
}

func assertReportedUsage(t *testing.T, interp task.Interpretation) {
	t.Helper()
	if len(interp.Usage) != 1 {
		t.Fatalf("usage=%+v", interp.Usage)
	}
	usage := interp.Usage[0]
	if usage.Scope != task.UsageScopeConversationCumulative || usage.Source != task.UsageSourceProviderEnvelope || usage.Reliability != task.UsageReliabilityReported {
		t.Fatalf("usage identity=%+v", usage)
	}
	assertCounter(t, "input", usage.InputTokens, 0)
	assertCounter(t, "output", usage.OutputTokens, 25)
	assertCounter(t, "thinking", usage.ThinkingTokens, 7)
	assertCounter(t, "total", usage.TotalTokens, 32)
	if usage.CacheReadTokens != nil {
		t.Fatalf("null cache counter became zero: %+v", usage)
	}
	assertCounter(t, "turns", usage.NumTurns, 3)
	if usage.DurationSeconds == nil || *usage.DurationSeconds != "1.250e+0" {
		t.Fatalf("duration=%v", usage.DurationSeconds)
	}
}

func assertReadersDrained(t *testing.T, stdout, stderr *trackingReader, stdoutLength, stderrLength int) {
	t.Helper()
	if stdout.off != stdoutLength || !stdout.eof || stderr.off != stderrLength || !stderr.eof {
		t.Fatalf("evidence was not drained: stdout=%+v stderr=%+v", stdout, stderr)
	}
}

func TestLongResponseHasNoScannerLimit(t *testing.T) {
	response := strings.Repeat("界", 40*1024) + strings.Repeat("x", 17*1024) + "\n"
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	stdout := envelopeWithResponse(testConversation, StatusSuccess, encoded, `,"usage":{}`)
	interp, answer, reader, _, err := evaluateAntigravity(t, stdout, nil, nil, task.SessionExpectation{}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictCommitted || !bytes.Equal(answer, []byte(response)) {
		t.Fatalf("verdict=%+v answer length=%d want=%d", interp, len(answer), len(response))
	}
	if reader.off != len(stdout) || !reader.eof {
		t.Fatalf("long stdout was not drained: %+v", reader)
	}
	if len(interp.Usage) != 1 {
		t.Fatalf("empty usage object was discarded: %+v", interp.Usage)
	}
}

func TestLongUnicodeResponseKeepsWritesBounded(t *testing.T) {
	response := strings.Repeat("界", 20*1024)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	writer := &trackingWriter{}
	interp, _, stdoutReader, stderrReader, err := evaluateAntigravityWithWriter(t, envelopeWithResponse(testConversation, StatusSuccess, encoded, ""), nil, nil, task.SessionExpectation{}, nil, 1, writer)
	if err != nil || interp.Verdict != task.VerdictCommitted {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
	if writer.String() != response {
		t.Fatalf("answer length=%d want=%d", writer.Len(), len(response))
	}
	if writer.maxWrite > responseBufferBytes {
		t.Fatalf("response write exceeded fixed buffer: max=%d limit=%d", writer.maxWrite, responseBufferBytes)
	}
	if stdoutReader.off == 0 || !stdoutReader.eof || !stderrReader.eof {
		t.Fatalf("evidence was not drained: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}
}

func TestUsagePresenceAndCumulativeResetArePreserved(t *testing.T) {
	withoutUsage := parseEnvelopeFixture(t, envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""))
	if usage := usageFromEnvelope(withoutUsage, task.UsageReliabilityReported); usage != nil {
		t.Fatalf("missing usage became available: %+v", usage)
	}
	withNull := parseEnvelopeFixture(t, envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), `,"usage":null`))
	if usage := usageFromEnvelope(withNull, task.UsageReliabilityReported); usage != nil {
		t.Fatalf("null usage became available: %+v", usage)
	}
	withZero := parseEnvelopeFixture(t, envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), `,"usage":{"total_tokens":0}`))
	zeroUsage := usageFromEnvelope(withZero, task.UsageReliabilityReported)
	if len(zeroUsage) != 1 || zeroUsage[0].TotalTokens == nil || *zeroUsage[0].TotalTokens != 0 {
		t.Fatalf("explicit zero usage changed: %+v", zeroUsage)
	}
	for _, want := range []int64{120, 3} {
		parsed := parseEnvelopeFixture(t, envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), fmt.Sprintf(`,"usage":{"total_tokens":%d}`, want)))
		usage := usageFromEnvelope(parsed, task.UsageReliabilityReported)
		if len(usage) != 1 || usage[0].TotalTokens == nil || *usage[0].TotalTokens != want || usage[0].Scope != task.UsageScopeConversationCumulative {
			t.Fatalf("cumulative usage=%+v want=%d", usage, want)
		}
	}
}

func parseEnvelopeFixture(t *testing.T, data []byte) envelope {
	t.Helper()
	parsed := parseStdout(bytes.NewReader(data), io.Discard)
	if parsed.semantic != nil || parsed.operation != nil {
		t.Fatalf("fixture parse failed: semantic=%v operation=%v", parsed.semantic, parsed.operation)
	}
	return parsed.envelope
}

func TestMalformedUTF8IsRejectedAfterDrain(t *testing.T) {
	validPrefix := []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"`)
	cases := []struct {
		name string
		data []byte
	}{
		{name: "invalid byte", data: append(append([]byte{}, validPrefix...), append([]byte{0xff}, []byte(`"}`)...)...)},
		{name: "truncated sequence", data: append(append([]byte{}, validPrefix...), append([]byte{0xe2, 0x82}, []byte(`"}`)...)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interp, _, reader, stderr, err := evaluateAntigravity(t, tc.data, []byte("stderr fully read"), nil, task.SessionExpectation{}, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != task.VerdictRejected || interp.Refusal != refusalInvalidOutput {
				t.Fatalf("interpretation=%+v", interp)
			}
			if reader.off != len(tc.data) || !reader.eof || stderr.off != len("stderr fully read") || !stderr.eof {
				t.Fatalf("semantic fault did not drain evidence: stdout=%+v stderr=%+v", reader, stderr)
			}
		})
	}
}

func TestEnvelopeGrammarRejectsDuplicatesUnknownTypesAndTrailingData(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
	}{
		{name: "duplicate status", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","status":"SUCCESS","response":"answer"}`)},
		{name: "escaped duplicate status", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","st\u0061tus":"SUCCESS","status":"SUCCESS","response":"answer"}`)},
		{name: "duplicate usage counter", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","usage":{"input_tokens":1,"in\u0070ut_tokens":2}}`)},
		{name: "unknown field", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","extra":1}`)},
		{name: "missing response", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS"}`)},
		{name: "null status", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":null,"response":"answer"}`)},
		{name: "wrong response type", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":7}`)},
		{name: "wrong ID type", stdout: []byte(`{"conversation_id":7,"status":"SUCCESS","response":"answer"}`)},
		{name: "wrong usage type", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","usage":"later"}`)},
		{name: "fractional turns", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","num_turns":1.5}`)},
		{name: "negative counter", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","usage":{"output_tokens":-1}}`)},
		{name: "nonfinite duration", stdout: []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","duration_seconds":1e999}`)},
		{name: "second object", stdout: append(envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""), []byte(`{}`)...)},
		{name: "trailing scalar", stdout: append(envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""), []byte(`1`)...)},
		{name: "non JSON whitespace", stdout: append(envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""), []byte{0xc2, 0xa0}...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interp, _, reader, stderr, err := evaluateAntigravity(t, tc.stdout, []byte("diagnostic"), nil, task.SessionExpectation{}, nil, 2)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != task.VerdictRejected || interp.Refusal != refusalInvalidOutput {
				t.Fatalf("interpretation=%+v", interp)
			}
			if reader.off != len(tc.stdout) || !reader.eof || stderr.off != len("diagnostic") || !stderr.eof {
				t.Fatalf("evidence not drained: stdout=%+v stderr=%+v", reader, stderr)
			}
		})
	}

	valid := append(envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""), []byte(" \t\r\n")...)
	interp, answer, _, _, err := evaluateAntigravity(t, valid, nil, nil, task.SessionExpectation{}, nil, 3)
	if err != nil || interp.Verdict != task.VerdictCommitted || string(answer) != "answer" {
		t.Fatalf("JSON whitespace positive control: interpretation=%+v answer=%q err=%v", interp, answer, err)
	}
}

func TestStatusExitAndSealErrorMatrix(t *testing.T) {
	valid := envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), "")
	cases := []struct {
		name       string
		stdout     []byte
		stderr     []byte
		alter      func(*task.ProviderExitRecord)
		wantReason string
		wantUsage  string
	}{
		{name: "provider error diagnostic", stdout: envelopeWithError(testConversation, StatusError, "failed"), wantReason: refusalProviderFailed + ": status=ERROR: failed", wantUsage: task.UsageReliabilityUnreliable},
		{name: "provider error without diagnostic", stdout: envelopeWithResponse(testConversation, StatusError, []byte(`"answer"`), ""), wantReason: refusalProviderFailed + ": status=ERROR", wantUsage: ""},
		{name: "unknown status", stdout: envelopeWithResponse(testConversation, "PAUSED", []byte(`"answer"`), `,"usage":{"total_tokens":2}`), wantReason: refusalInvalidOutput, wantUsage: task.UsageReliabilityUnreliable},
		{name: "success with error payload", stdout: envelopeWithError(testConversation, StatusSuccess, "contradiction"), wantReason: refusalInvalidOutput, wantUsage: task.UsageReliabilityUnreliable},
		{name: "nonzero exit", stdout: valid, alter: func(s *task.ProviderExitRecord) { s.ExitCode = 7 }, wantReason: refusalProviderFailed + ": exit_code=7", wantUsage: ""},
		{name: "signal exit observation", stdout: valid, alter: func(s *task.ProviderExitRecord) { s.ExitCode = -1 }, wantReason: refusalProviderFailed + ": exit_code=-1", wantUsage: ""},
		{name: "start failure", stdout: valid, alter: func(s *task.ProviderExitRecord) {
			s.InvocationState, s.Error, s.ExitCode = task.InvocationStartFailed, "launch failed", 1
		}, wantReason: refusalProviderFailed + ": start_failed", wantUsage: ""},
		{name: "sealed capture error", stdout: valid, alter: func(s *task.ProviderExitRecord) { s.Error = "capture failed" }, wantReason: refusalProviderFailed + ": seal_error", wantUsage: ""},
		{name: "empty success", stdout: envelopeWithResponse(testConversation, StatusSuccess, []byte(`" \t\n"`), ""), wantReason: refusalIncompleteOutput, wantUsage: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interp, _, _, _, err := evaluateAntigravity(t, tc.stdout, tc.stderr, tc.alter, task.SessionExpectation{}, nil, 4)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != task.VerdictRejected || interp.Refusal != tc.wantReason {
				t.Fatalf("interpretation=%+v", interp)
			}
			if tc.wantUsage == "" && len(interp.Usage) != 0 {
				t.Fatalf("unexpected usage=%+v", interp.Usage)
			}
			if tc.wantUsage != "" && (len(interp.Usage) != 1 || interp.Usage[0].Reliability != tc.wantUsage) {
				t.Fatalf("usage=%+v want reliability=%q", interp.Usage, tc.wantUsage)
			}
		})
	}
}

func TestIdentityExpectationsAndRecordedIdentity(t *testing.T) {
	valid := envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), "")
	cases := []struct {
		name     string
		expected task.SessionExpectation
		recorded *task.SessionIdentity
		alter    func(*task.ProviderExitRecord)
		want     string
	}{
		{name: "fresh", want: task.VerdictCommitted},
		{name: "exact continuation", expected: task.SessionExpectation{Required: true, ID: testConversation}, want: task.VerdictCommitted},
		{name: "wrong continuation", expected: task.SessionExpectation{Required: true, ID: "123e4567-e89b-12d3-a456-426614174001"}, want: refusalIdentityMismatch},
		{name: "matching recorded", recorded: &task.SessionIdentity{Provider: Provider, ConversationID: testConversation}, want: task.VerdictCommitted},
		{name: "wrong recorded provider", recorded: &task.SessionIdentity{Provider: "fixture:test", ConversationID: testConversation}, want: refusalIdentityMismatch},
		{name: "wrong recorded ID", recorded: &task.SessionIdentity{Provider: Provider, ConversationID: "123e4567-e89b-12d3-a456-426614174001"}, want: refusalIdentityMismatch},
		{name: "malformed ID", want: refusalIdentityMismatch, alter: func(s *task.ProviderExitRecord) { _ = s }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := valid
			if tc.name == "malformed ID" {
				stdout = envelopeWithResponse("bad/id", StatusSuccess, []byte(`"answer"`), "")
			}
			interp, _, _, _, err := evaluateAntigravity(t, stdout, nil, tc.alter, tc.expected, tc.recorded, 5)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == task.VerdictCommitted {
				if interp.Verdict != task.VerdictCommitted || interp.Session == nil {
					t.Fatalf("interpretation=%+v", interp)
				}
			} else if interp.Verdict != task.VerdictRejected || interp.Refusal != tc.want || interp.Session != nil {
				t.Fatalf("interpretation=%+v", interp)
			}
		})
	}
}

func TestTimeoutMarkerSplitAtEveryBoundary(t *testing.T) {
	marker := []byte(TimeoutMarker)
	for split := 0; split <= len(marker); split++ {
		t.Run(fmt.Sprintf("split-%02d", split), func(t *testing.T) {
			reader := &trackingReader{data: append([]byte("prefix"), append(append([]byte{}, marker...), []byte("suffix")...)...), chunk: split + 1}
			assertTimeoutFound(t, reader)
		})
	}
}

func assertTimeoutFound(t *testing.T, reader *trackingReader) {
	t.Helper()
	found, err := scanTimeout(reader)
	if err != nil || !found || reader.off != len(reader.data) || !reader.eof {
		t.Fatalf("found=%v err=%v reader=%+v", found, err, reader)
	}
}

func TestTimeoutNearMissesAndWrongStream(t *testing.T) {
	for _, nearMiss := range [][]byte{
		[]byte("[agy] print timeou"),
		[]byte("[AGY] print timeout"),
		[]byte("[agy] print time-out"),
	} {
		found, err := scanTimeout(&trackingReader{data: nearMiss, chunk: 1})
		if err != nil || found {
			t.Fatalf("near miss %q found=%v err=%v", nearMiss, found, err)
		}
	}
	stdout := envelopeWithResponse(testConversation, StatusSuccess, []byte(jsonQuote("answer "+TimeoutMarker)), "")
	interp, answer, _, _, err := evaluateAntigravity(t, stdout, nil, nil, task.SessionExpectation{}, nil, 2)
	if err != nil || interp.Verdict != task.VerdictCommitted || string(answer) != "answer "+TimeoutMarker {
		t.Fatalf("stdout marker changed interpretation=%+v err=%v", interp, err)
	}
}

func TestTimeoutMarkerRefusesSuccessAndMarksUsageUnreliable(t *testing.T) {
	stdout := envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), `,"usage":{"total_tokens":0}`)
	interp, _, _, _, err := evaluateAntigravity(t, stdout, []byte("prefix"+TimeoutMarker+"suffix"), nil, task.SessionExpectation{}, nil, 2)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalIncompleteOutput || len(interp.Usage) != 1 || interp.Usage[0].Reliability != task.UsageReliabilityUnreliable {
		t.Fatalf("timeout interpretation=%+v err=%v", interp, err)
	}
}

func TestSemanticFaultAndWriterFaultDrainBothStreams(t *testing.T) {
	semanticStdout := append([]byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"answer","status":"SUCCESS"}`), bytes.Repeat([]byte("x"), 128*1024)...)
	interp, _, stdoutReader, stderrReader, err := evaluateAntigravity(t, semanticStdout, bytes.Repeat([]byte("e"), 64*1024), nil, task.SessionExpectation{}, nil, 1)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalInvalidOutput {
		t.Fatalf("semantic interpretation=%+v err=%v", interp, err)
	}
	if stdoutReader.off != len(semanticStdout) || !stdoutReader.eof || stderrReader.off != 64*1024 || !stderrReader.eof {
		t.Fatalf("semantic fault did not drain streams: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}

	longResponse, err := json.Marshal(strings.Repeat("answer", 20*1024))
	if err != nil {
		t.Fatal(err)
	}
	stdout := envelopeWithResponse(testConversation, StatusSuccess, longResponse, "")
	writerErr := errors.New("answer writer failed")
	failing := &boundedFailWriter{limit: 100, err: writerErr}
	interp, _, stdoutReader, stderrReader, err = evaluateAntigravityWithWriter(t, stdout, []byte("stderr"), nil, task.SessionExpectation{}, nil, 3, failing)
	if !errors.Is(err, writerErr) || interp.Verdict != "" {
		t.Fatalf("writer fault interpretation=%+v err=%v", interp, err)
	}
	if stdoutReader.off != len(stdout) || !stdoutReader.eof || stderrReader.off != len("stderr") || !stderrReader.eof {
		t.Fatalf("writer fault did not drain streams: stdout=%+v stderr=%+v", stdoutReader, stderrReader)
	}
}

func TestEvidenceReadErrorsRemainOperational(t *testing.T) {
	readErr := errors.New("stderr read failed")
	stdoutReader := &trackingReader{data: envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), ""), chunk: 2}
	stderrReader := &errorReader{data: []byte("diagnostic"), err: readErr, chunk: 2}
	evidence := &testEvidence{readers: map[predicate.Stream]io.Reader{predicate.Stdout: stdoutReader, predicate.Stderr: stderrReader}}
	seal := validSeal(NewPrintInterpreter().Reference())
	var answer bytes.Buffer
	interp, err := NewPrintInterpreter().Evaluate(predicate.Input{Seal: seal}, evidence, &answer)
	if !errors.Is(err, readErr) || interp.Verdict != "" {
		t.Fatalf("interpretation=%+v err=%v", interp, err)
	}
}

func envelopeWithResponse(id, status string, responseJSON []byte, extras string) []byte {
	return []byte(`{"conversation_id":` + jsonQuote(id) + `,"status":` + jsonQuote(status) + `,"response":` + string(responseJSON) + extras + `}`)
}

func envelopeWithError(id, status, diagnostic string) []byte {
	return []byte(`{"conversation_id":` + jsonQuote(id) + `,"status":` + jsonQuote(status) + `,"response":"answer","error":` + jsonQuote(diagnostic) + `,"usage":{"total_tokens":2}}`)
}

func jsonQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func evaluateAntigravity(t *testing.T, stdout, stderr []byte, alter func(*task.ProviderExitRecord), expected task.SessionExpectation, recorded *task.SessionIdentity, chunk int) (task.Interpretation, []byte, *trackingReader, *trackingReader, error) {
	return evaluateAntigravityWithWriter(t, stdout, stderr, alter, expected, recorded, chunk, &bytes.Buffer{})
}

func evaluateAntigravityWithWriter(t *testing.T, stdout, stderr []byte, alter func(*task.ProviderExitRecord), expected task.SessionExpectation, recorded *task.SessionIdentity, chunk int, writer io.Writer) (task.Interpretation, []byte, *trackingReader, *trackingReader, error) {
	t.Helper()
	stdoutReader := &trackingReader{data: stdout, chunk: chunk}
	stderrReader := &trackingReader{data: stderr, chunk: chunk}
	evidence := &testEvidence{readers: map[predicate.Stream]io.Reader{predicate.Stdout: stdoutReader, predicate.Stderr: stderrReader}}
	seal := validSeal(NewPrintInterpreter().Reference())
	if alter != nil {
		alter(&seal)
	}
	if buffer, ok := writer.(*bytes.Buffer); ok {
		result, err := NewPrintInterpreter().Evaluate(predicate.Input{Seal: seal, ExpectedSession: expected, RecordedSession: recorded}, evidence, buffer)
		return result, buffer.Bytes(), stdoutReader, stderrReader, err
	}
	result, err := NewPrintInterpreter().Evaluate(predicate.Input{Seal: seal, ExpectedSession: expected, RecordedSession: recorded}, evidence, writer)
	return result, nil, stdoutReader, stderrReader, err
}

func validSeal(ref task.PredicateRef) task.ProviderExitRecord {
	raw := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: task.ComputeSHA256(nil)},
		{Path: "raw/stdout", Size: 0, SHA256: task.ComputeSHA256(nil)},
	}
	manifest, err := task.MarshalCanonical(raw)
	if err != nil {
		panic(err)
	}
	return task.ProviderExitRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          testRootID,
		TaskID:          testTaskID,
		SpecSHA256:      testSpecSHA256,
		MetaSHA256:      testMetaSHA256,
		InvocationState: task.InvocationStarted,
		ExitCode:        0,
		Predicate:       ref,
		RawManifest:     raw,
		ManifestSHA256:  task.ComputeSHA256(manifest),
		ClosedAt:        "2026-09-13T00:00:00Z",
	}
}

func assertCounter(t *testing.T, name string, actual *int64, want int64) {
	t.Helper()
	if actual == nil || *actual != want {
		t.Fatalf("%s counter=%v want=%d", name, actual, want)
	}
}

type testEvidence struct {
	readers map[predicate.Stream]io.Reader
}

func (e *testEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	reader, ok := e.readers[stream]
	if !ok {
		return fmt.Errorf("missing %v evidence", stream)
	}
	return consume(reader)
}

type trackingReader struct {
	data  []byte
	chunk int
	off   int
	eof   bool
}

func (r *trackingReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		r.eof = true
		return 0, io.EOF
	}
	size := r.chunk
	if size <= 0 || size > len(p) {
		size = len(p)
	}
	if remaining := len(r.data) - r.off; size > remaining {
		size = remaining
	}
	copy(p[:size], r.data[r.off:r.off+size])
	r.off += size
	return size, nil
}

type errorReader struct {
	data  []byte
	err   error
	chunk int
	off   int
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, r.err
	}
	size := r.chunk
	if size <= 0 || size > len(p) {
		size = len(p)
	}
	if remaining := len(r.data) - r.off; size > remaining {
		size = remaining
	}
	copy(p[:size], r.data[r.off:r.off+size])
	r.off += size
	return size, nil
}

type boundedFailWriter struct {
	limit int
	wrote int
	err   error
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

type trackingWriter struct {
	bytes.Buffer
	maxWrite int
}

func (w *trackingWriter) Write(data []byte) (int, error) {
	if len(data) > w.maxWrite {
		w.maxWrite = len(data)
	}
	return w.Buffer.Write(data)
}

// The live permission failure reported denied_actions beside a SUCCESS status.
// A partial answer must not turn this refusal-shaped envelope into completion.
func TestObservedDeniedActionWithPartialAnswerIsRejected(t *testing.T) {
	stdout := []byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000","status":"SUCCESS","response":"I started the task.","denied_actions":[{"action":"read_file","display_name":"ViewFile"}]}`)
	interp, _, reader, stderr, err := evaluateAntigravity(t, stdout, nil, nil, task.SessionExpectation{}, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictRejected || interp.Session != nil {
		t.Fatalf("denied action was publishable: %+v", interp)
	}
	assertReadersDrained(t, reader, stderr, len(stdout), 0)
}
