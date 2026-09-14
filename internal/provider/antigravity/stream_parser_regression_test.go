package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrettyEnvelopeWhitespaceIsAcceptedButSurrogateWhitespaceIsNot(t *testing.T) {
	stdout := []byte("{\n" +
		"  \"status\" \t : \n \"SUCCESS\" ,\n" +
		"  \"conversation_id\" \n : \n \"" + testConversation + "\" ,\n" +
		"  \"response\" \n : \n \"answer \\ud83d\\ude00\" ,\n" +
		"  \"duration_seconds\" \n : \n 1.5 ,\n" +
		"  \"num_turns\" \n : \n 2 ,\n" +
		"  \"usage\" \n : {\n" +
		"    \"input_tokens\" \n : \n 1 ,\n" +
		"    \"output_tokens\" \n : \n 2\n" +
		"  }\n" +
		"}\n")
	interp, answer, stdoutReader, stderrReader, err := evaluateAntigravity(t, stdout, nil, nil, task.SessionExpectation{}, nil, 1)
	if err != nil || interp.Verdict != task.VerdictCommitted || string(answer) != "answer 😀" {
		t.Fatalf("pretty envelope interpretation=%+v answer=%q err=%v", interp, answer, err)
	}
	assertReadersDrained(t, stdoutReader, stderrReader, len(stdout), 0)

	withWhitespaceInSurrogate := bytes.Replace(stdout, []byte(`\ud83d\ude00`), []byte(`\ud83d \ude00`), 1)
	interp, _, stdoutReader, stderrReader, err = evaluateAntigravity(t, withWhitespaceInSurrogate, nil, nil, task.SessionExpectation{}, nil, 1)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalInvalidOutput {
		t.Fatalf("surrogate whitespace interpretation=%+v err=%v", interp, err)
	}
	assertReadersDrained(t, stdoutReader, stderrReader, len(withWhitespaceInSurrogate), 0)
}

func TestResponseSinkRetainsBatchCapacityAndExactOutput(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), responseBufferBytes*4+123)
	writer := &countingResponseWriter{}
	sink := newResponseSink(writer)
	for _, value := range payload {
		if err := sink.emit([]byte{value}, rune(value)); err != nil {
			t.Fatal(err)
		}
	}
	sink.flush()
	if sink.writerErr != nil {
		t.Fatal(sink.writerErr)
	}
	if !bytes.Equal(writer.Bytes(), payload) {
		t.Fatalf("response bytes changed: got=%d want=%d", writer.Len(), len(payload))
	}
	if cap(sink.buffer) < responseBufferBytes {
		t.Fatalf("response buffer capacity was not retained: cap=%d", cap(sink.buffer))
	}
	maxWrites := (len(payload)+responseBufferBytes-1)/responseBufferBytes + 1
	if writer.writes > maxWrites {
		t.Fatalf("response batching regressed: writes=%d bound=%d", writer.writes, maxWrites)
	}
}

func TestResponseSinkHandlesPartialWritesAndShortWriteDrain(t *testing.T) {
	payload := bytes.Repeat([]byte("partial-write"), responseBufferBytes/len("partial-write")+37)
	partial := &partialResponseWriter{chunk: 137}
	sink := newResponseSink(partial)
	if err := sink.emit(payload, 'x'); err != nil {
		t.Fatal(err)
	}
	sink.flush()
	if sink.writerErr != nil || !bytes.Equal(partial.Bytes(), payload) {
		t.Fatalf("partial writer result err=%v bytes=%d want=%d", sink.writerErr, partial.Len(), len(payload))
	}
	if cap(sink.buffer) < responseBufferBytes || len(sink.buffer) != 0 {
		t.Fatalf("partial flush did not reset reusable buffer: len=%d cap=%d", len(sink.buffer), cap(sink.buffer))
	}

	response := strings.Repeat("drain", responseBufferBytes)
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	stdout := envelopeWithResponse(testConversation, StatusSuccess, encodedResponse, "")
	reader := &trackingReader{data: stdout, chunk: 11}
	short := &shortResponseWriter{remaining: 37}
	result := parseStdout(reader, short)
	if result.semantic != nil || !errors.Is(result.operation, io.ErrShortWrite) {
		t.Fatalf("short writer result semantic=%v operation=%v", result.semantic, result.operation)
	}
	if reader.off != len(stdout) || !reader.eof {
		t.Fatalf("short writer did not drain stdout: %+v", reader)
	}
	if !bytes.Equal(short.Bytes(), []byte(response[:37])) {
		t.Fatalf("short writer output changed: got=%d want=%d", short.Len(), 37)
	}
}

func TestDurationRejectsNegativeUnderflowAndAllowsSignedZero(t *testing.T) {
	underflow := envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), `,"duration_seconds":-1e-999`)
	interp, _, _, _, err := evaluateAntigravity(t, underflow, nil, nil, task.SessionExpectation{}, nil, 1)
	if err != nil || interp.Verdict != task.VerdictRejected || interp.Refusal != refusalInvalidOutput {
		t.Fatalf("negative underflow interpretation=%+v err=%v", interp, err)
	}

	for _, zero := range []string{"-0", "-0.0", "-0e-999"} {
		t.Run(zero, func(t *testing.T) {
			stdout := envelopeWithResponse(testConversation, StatusSuccess, []byte(`"answer"`), `,"duration_seconds":`+zero)
			interp, answer, _, _, err := evaluateAntigravity(t, stdout, nil, nil, task.SessionExpectation{}, nil, 1)
			if err != nil || interp.Verdict != task.VerdictCommitted || string(answer) != "answer" {
				t.Fatalf("signed zero interpretation=%+v answer=%q err=%v", interp, answer, err)
			}
		})
	}
}

type countingResponseWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(data)
}

type partialResponseWriter struct {
	bytes.Buffer
	chunk  int
	writes int
}

func (w *partialResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	n := w.chunk
	if n > len(data) {
		n = len(data)
	}
	if n <= 0 {
		return 0, nil
	}
	if _, err := w.Buffer.Write(data[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

type shortResponseWriter struct {
	bytes.Buffer
	remaining int
}

func (w *shortResponseWriter) Write(data []byte) (int, error) {
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
