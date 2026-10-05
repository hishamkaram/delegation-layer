package antigravity

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

type recordEvidence struct{ stdout, stderr string }

func (e recordEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if stream == predicate.Stdout {
		return consume(strings.NewReader(e.stdout))
	}
	return consume(strings.NewReader(e.stderr))
}

func TestRecordEnvelopeExtensionsAndAccounting(t *testing.T) {
	for _, usage := range []string{`{"input_tokens":1,"new_usage":{"id":1,"id":2}}`, `{"input_tokens":"unavailable"}`, `{"input_tokens":1,"input_tokens":2}`} {
		t.Run(usage, func(t *testing.T) {
			interpreter := NewRecordPrintInterpreter(ModeWorkspaceWrite)
			stdout := `{"response":"answer","conversation_id":"` + testConversation + `","extension":{"id":1,"id":2},"usage":` + usage + `,"status":"SUCCESS"}`
			var answer bytes.Buffer
			result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, recordEvidence{stdout: stdout}, &answer)
			if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != "answer" {
				t.Fatalf("result=%+v answer=%q err=%v", result, answer.String(), err)
			}
			if strings.Contains(usage, "unavailable") || strings.Count(usage, "input_tokens") == 2 {
				if len(result.Usage) != 0 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "optional_usage_unavailable" {
					t.Fatalf("invalid accounting not isolated: %+v", result)
				}
			}
			if err = task.ValidateInterpretation(result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecordEnvelopeDoesNotReportEmptyUsage(t *testing.T) {
	for _, usage := range []string{`{}`, `{"unrecognized_tokens":1}`} {
		t.Run(usage, func(t *testing.T) {
			interpreter := NewRecordPrintInterpreter(ModeWorkspaceWrite)
			stdout := `{"response":"answer","conversation_id":"` + testConversation + `","usage":` + usage + `,"status":"SUCCESS"}`
			result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, recordEvidence{stdout: stdout}, io.Discard)
			if err != nil || result.Verdict != task.VerdictCommitted || len(result.Usage) != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestRecordEnvelopeRejectsAuthorityAmbiguity(t *testing.T) {
	for _, fields := range []string{`"status":"SUCCESS","status":"SUCCESS"`, `"status":"SUCCESS","STATUS":"ERROR"`, `"status":"SUCCESS","extension":{"bad":[1,]}`} {
		interpreter := NewRecordPrintInterpreter(ModeReadOnly)
		stdout := `{"response":"answer","conversation_id":"` + testConversation + `",` + fields + `}`
		result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, recordEvidence{stdout: stdout}, io.Discard)
		if err != nil || result.Verdict != task.VerdictRejected {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestRecordEnvelopeStreamsLargeAnswerAndDiscardsLargeExtension(t *testing.T) {
	interpreter := NewRecordPrintInterpreter(ModeWorkspaceWrite)
	large := strings.Repeat("a", 32<<20)
	stdout := `{"extension":"` + large + `","response":"` + large + `","status":"SUCCESS","conversation_id":"` + testConversation + `"}`
	writer := &boundedRecordWriter{}
	result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, recordEvidence{stdout: stdout}, writer)
	if err != nil || result.Verdict != task.VerdictCommitted || writer.count != len(large) || writer.max > 32<<10 {
		t.Fatalf("result=%+v writer=%+v err=%v", result, writer, err)
	}
}

type boundedRecordWriter struct{ count, max int }

func (w *boundedRecordWriter) Write(p []byte) (int, error) {
	w.count += len(p)
	w.max = max(w.max, len(p))
	return len(p), nil
}

func TestRecordRecoveryPreservesSourcePredicate(t *testing.T) {
	interpreter := recordPrintInterpreter{mode: ModeWorkspaceWrite}
	source := NewCurrentPrintInterpreter().Reference()
	input := predicate.Input{Seal: validSeal(source)}
	stdout := `{"response":"recovered","conversation_id":"` + testConversation + `","extension":{"id":1,"id":2},"status":"SUCCESS"}`
	var answer bytes.Buffer
	result, err := interpreter.Recover(input, recordEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != "recovered" || !input.Seal.Predicate.Equal(source) {
		t.Fatalf("result=%+v error=%v source=%+v", result, err, input.Seal.Predicate)
	}
	input.Seal.Predicate = interpreter.Reference()
	if _, err = interpreter.Recover(input, recordEvidence{stdout: stdout}, io.Discard); err == nil {
		t.Fatal("same revision recovery must be unsupported")
	}
}

func TestRecordEnvelopeReaderAndWriterFailuresAreOperational(t *testing.T) {
	interpreter := NewRecordPrintInterpreter(ModeWorkspaceWrite)
	stdout := `{"response":"answer","conversation_id":"` + testConversation + `","status":"SUCCESS"}`
	failure := errors.New("injected I/O failure")
	input := predicate.Input{Seal: validSeal(interpreter.Reference())}
	if _, err := interpreter.Evaluate(input, recordReadFaultEvidence{stdout: stdout, err: failure}, io.Discard); !errors.Is(err, failure) {
		t.Fatalf("record read error=%v", err)
	}
	if _, err := interpreter.Evaluate(input, recordEvidence{stdout: stdout}, recordFailWriter{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("answer writer error=%v", err)
	}
}

type recordReadFaultEvidence struct {
	stdout string
	err    error
}

func (e recordReadFaultEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if stream == predicate.Stderr {
		return consume(strings.NewReader(""))
	}
	return consume(recordFaultReader{Reader: strings.NewReader(e.stdout), err: e.err})
}

type recordFaultReader struct {
	*strings.Reader
	err error
}

func (r recordFaultReader) ReadAt([]byte, int64) (int, error) { return 0, r.err }

type recordFailWriter struct{ err error }

func (w recordFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRecordDiagnosticsDoNotExposeProviderValues(t *testing.T) {
	interpreter := NewRecordPrintInterpreter(ModeWorkspaceWrite)
	for _, stdout := range []string{
		`{"response":"secret-answer","conversation_id":"` + testConversation + `","status":"ERROR","error":"SECRET_API_KEY"}`,
		`{"response":"secret-answer","conversation_id":"SECRET_SESSION","status":"SUCCESS"}`,
		`{"response":"secret-answer","conversation_id":"` + testConversation + `","status":"SUCCESS","status":"SECRET_STATUS"}`,
	} {
		result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, recordEvidence{stdout: stdout}, io.Discard)
		if err != nil || result.Verdict != task.VerdictRejected || len(result.Diagnostics) != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if strings.Contains(fmt.Sprint(result.Diagnostics, result.Refusal), "SECRET") {
			t.Fatalf("native value leaked into diagnostic: %+v", result)
		}
		if err = task.ValidateInterpretation(result); err != nil {
			t.Fatal(err)
		}
	}
}
