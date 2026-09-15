package fakeprovider

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

const (
	testRootID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSpec   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testMeta   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func TestPredicateReferenceAndContractDigest(t *testing.T) {
	ref := Predicate().Reference()
	want := task.PredicateRef{Adapter: Provider, Mode: Mode, Version: Version, SHA256: "8f42ad8ce7677acab2f694c1e014a918edaf8ae8fac7a4bbbdc51603c6adbe93"}
	if ref != want {
		t.Fatalf("reference = %+v, want %+v", ref, want)
	}
	if task.ComputeSHA256([]byte(Contract())) != ref.SHA256 || !strings.HasSuffix(Contract(), "\n") {
		t.Fatal("contract digest or terminator changed")
	}
}

func TestPredicateSuccessPreservesDecodedChunksExactly(t *testing.T) {
	stdout := sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "first\n") + answerFrame(testTaskID, "☃\\tail") + resultFrame(testTaskID, "conv-1", true)
	interp, answer, err := evaluateFixture(t, stdout, "ordinary diagnostic\n", func(seal *task.ProviderExitRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictCommitted || interp.Refusal != "" {
		t.Fatalf("interpretation = %+v", interp)
	}
	if string(answer) != "first\n☃\\tail" {
		t.Fatalf("answer = %q", answer)
	}
	if interp.Session == nil || *interp.Session != (task.SessionIdentity{Provider: Provider, ConversationID: "conv-1"}) {
		t.Fatalf("session = %+v", interp.Session)
	}
}

func TestPredicateRefusalPrecedence(t *testing.T) {
	valid := sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true)
	cases := []struct {
		name   string
		stdout string
		stderr string
		mutate func(*task.ProviderExitRecord)
		want   string
	}{
		{name: "start failure", stdout: valid, mutate: func(s *task.ProviderExitRecord) {
			s.InvocationState, s.Error, s.ExitCode = task.InvocationStartFailed, "launch failed", 7
		}, want: "start_failed: launch failed"},
		{name: "capture error", stdout: valid, mutate: func(s *task.ProviderExitRecord) { s.Error, s.ExitCode = "capture failed", 7 }, want: "capture_error: capture failed"},
		{name: "nonzero exit", stdout: valid, mutate: func(s *task.ProviderExitRecord) { s.ExitCode = 7 }, want: "provider_exit: 7"},
		{name: "stderr marker", stdout: valid, stderr: "prefixDELEGATE_FIXTURE_ERRORsuffix", want: "stderr_error"},
		{name: "stdout semantic", stdout: sessionFrame(testTaskID, "conv-1") + `{"type":"wat","task_id":"` + testTaskID + `"}` + "\n", want: "malformed_output"},
		{name: "missing session", stdout: "", want: "missing_session"},
		{name: "missing result", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer"), want: "missing_result"},
		{name: "empty answer", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, " \t\n") + resultFrame(testTaskID, "conv-1", true), want: "empty_answer"},
		{name: "commit", stdout: valid, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interp, answer, err := evaluateFixture(t, tc.stdout, tc.stderr, tc.mutate)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if interp.Verdict != task.VerdictCommitted || interp.Refusal != "" {
					t.Fatalf("interpretation = %+v", interp)
				}
				return
			}
			if interp.Verdict != task.VerdictRejected || interp.Refusal != tc.want || strings.HasSuffix(interp.Refusal, "\n") {
				t.Fatalf("interpretation = %+v, answer prefix=%q", interp, answer)
			}
		})
	}
}

func TestPredicateSchemaAndIdentityFaults(t *testing.T) {
	valid := sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true)
	cases := []struct {
		name   string
		stdout string
		input  func(*predicate.Input)
		want   string
	}{
		{name: "empty frame", stdout: "\n", want: "malformed_output"},
		{name: "truncated frame", stdout: strings.TrimSuffix(valid, "\n"), want: "malformed_output"},
		{name: "invalid utf8", stdout: string([]byte{'{', '\n', 0xff, '\n'}), want: "malformed_output"},
		{name: "non object", stdout: "[]\n", want: "malformed_output"},
		{name: "duplicate key", stdout: `{"type":"session","type":"session","task_id":"` + testTaskID + `","session_id":"conv-1"}` + "\n", want: "malformed_output"},
		{name: "unknown field", stdout: `{"type":"session","task_id":"` + testTaskID + `","session_id":"conv-1","extra":1}` + "\n", want: "malformed_output"},
		{name: "wrong case field", stdout: `{"Type":"session","task_id":"` + testTaskID + `","session_id":"conv-1"}` + "\n", want: "malformed_output"},
		{name: "missing field", stdout: `{"type":"session","task_id":"` + testTaskID + `"}` + "\n", want: "malformed_output"},
		{name: "null field", stdout: `{"type":"session","task_id":"` + testTaskID + `","session_id":null}` + "\n", want: "malformed_output"},
		{name: "trailing json", stdout: `{"type":"session","task_id":"` + testTaskID + `","session_id":"conv-1"}{}` + "\n", want: "malformed_output"},
		{name: "wrong type", stdout: `{"type":"session","task_id":7,"session_id":"conv-1"}` + "\n", want: "malformed_output"},
		{name: "oversize frame", stdout: strings.Repeat("x", maxFrameBytes+1) + "\n", want: "malformed_output"},
		{name: "task mismatch", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame("cccccccccccccccccccccccccccccccc", "answer") + resultFrame(testTaskID, "conv-1", true), want: "task_identity_mismatch"},
		{name: "session format", stdout: sessionFrame(testTaskID, "bad/id") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "bad/id", true), want: "session_identity_mismatch"},
		{name: "expected session mismatch", stdout: valid, input: func(in *predicate.Input) { in.ExpectedSession = task.SessionExpectation{Required: true, ID: "other"} }, want: "session_identity_mismatch"},
		{name: "recorded session mismatch", stdout: valid, input: func(in *predicate.Input) {
			in.RecordedSession = &task.SessionIdentity{Provider: Provider, ConversationID: "other"}
		}, want: "session_identity_mismatch"},
		{name: "duplicate session", stdout: sessionFrame(testTaskID, "conv-1") + sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true), want: "duplicate_session"},
		{name: "conflicting session after result", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true) + sessionFrame(testTaskID, "conv-2"), want: "session_identity_mismatch"},
		{name: "late session after result", stdout: answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true) + sessionFrame(testTaskID, "conv-1"), want: "session_identity_mismatch"},
		{name: "result before session", stdout: resultFrame(testTaskID, "conv-1", true) + sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer"), want: "session_identity_mismatch"},
		{name: "event after result", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true) + answerFrame(testTaskID, "late"), want: "event_after_result"},
		{name: "provider failure", stdout: sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", false), want: "provider_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interp, _, err := evaluateFixtureWithInput(t, tc.stdout, "", tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if interp.Verdict != task.VerdictRejected || interp.Refusal != tc.want {
				t.Fatalf("interpretation = %+v, want refusal %q", interp, tc.want)
			}
		})
	}
}

func TestPredicateReadsBothStreamsAndHandlesLargeTotalOutput(t *testing.T) {
	var stdout strings.Builder
	stdout.WriteString(sessionFrame(testTaskID, "conv-1"))
	for i := 0; i < 40; i++ {
		stdout.WriteString(answerFrame(testTaskID, strings.Repeat("x", 32*1024)))
	}
	stdout.WriteString(resultFrame(testTaskID, "conv-1", true))
	stderr := strings.Repeat("e", 2*1024*1024)
	interp, answer, err := evaluateFixtureWithReaders(t, &chunkReader{data: []byte(stdout.String()), size: 1}, &chunkReader{data: []byte(stderr), size: 17}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictCommitted || len(answer) != 40*32*1024 {
		t.Fatalf("verdict=%+v answer=%d", interp, len(answer))
	}
}

func TestPredicateFindsSplitStderrMarkerAndValidatesFrameBoundary(t *testing.T) {
	valid := sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true)
	marker := []byte("prefix" + stderrMarker + "suffix")
	interp, _, err := evaluateFixtureWithReaders(t, &chunkReader{data: []byte(valid), size: maxFrameBytes}, &chunkReader{data: marker, size: len("prefix") + 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictRejected || interp.Refusal != "stderr_error" {
		t.Fatalf("interpretation=%+v", interp)
	}

	base := len(strings.TrimSuffix(answerFrame(testTaskID, ""), "\n"))
	boundary := strings.Repeat("a", maxFrameBytes-base)
	interp, _, err = evaluateFixture(t, sessionFrame(testTaskID, "conv-1")+answerFrame(testTaskID, boundary)+resultFrame(testTaskID, "conv-1", true), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if interp.Verdict != task.VerdictCommitted {
		t.Fatalf("exact frame boundary interpretation=%+v", interp)
	}
}

func TestPredicateInfrastructureErrorsSupersedeSemanticFaults(t *testing.T) {
	readFailure := errors.New("source read failed")
	evidence := testEvidence{streams: map[predicate.Stream]io.Reader{
		predicate.Stdout: bytes.NewBufferString(sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer")),
		predicate.Stderr: &failingReader{data: []byte("DELEGATE_FIXTURE_ERROR"), err: readFailure},
	}}
	seal := validSeal(testTaskID, Predicate().Reference())
	var answer bytes.Buffer
	_, err := Predicate().Evaluate(predicate.Input{Seal: seal}, evidence, &answer)
	if !errors.Is(err, readFailure) {
		t.Fatalf("error=%v, want source read error", err)
	}

	writeFailure := errors.New("sink write failed")
	evidence = testEvidence{streams: map[predicate.Stream]io.Reader{
		predicate.Stdout: bytes.NewBufferString(sessionFrame(testTaskID, "conv-1") + answerFrame(testTaskID, "answer") + resultFrame(testTaskID, "conv-1", true)),
		predicate.Stderr: bytes.NewBufferString("marker DELEGATE_FIXTURE_ERROR"),
	}}
	_, err = Predicate().Evaluate(predicate.Input{Seal: seal}, evidence, failingWriter{err: writeFailure})
	if !errors.Is(err, writeFailure) {
		t.Fatalf("error=%v, want sink write error", err)
	}
}

func TestPredicateRejectsSealReferenceMismatch(t *testing.T) {
	_, _, err := evaluateFixture(t, "", "", func(s *task.ProviderExitRecord) { s.Predicate.Version = "1" })
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("error=%v, want identity mismatch", err)
	}
}

func evaluateFixture(t *testing.T, stdout, stderr string, mutate func(*task.ProviderExitRecord)) (task.Interpretation, []byte, error) {
	t.Helper()
	return evaluateFixtureWithReaders(t, bytes.NewBufferString(stdout), bytes.NewBufferString(stderr), mutate)
}

func evaluateFixtureWithInput(t *testing.T, stdout, stderr string, alter func(*predicate.Input)) (task.Interpretation, []byte, error) {
	t.Helper()
	return evaluateWithInput(t, bytes.NewBufferString(stdout), bytes.NewBufferString(stderr), alter, nil)
}

func evaluateFixtureWithReaders(t *testing.T, stdout, stderr io.Reader, alter func(*task.ProviderExitRecord)) (task.Interpretation, []byte, error) {
	t.Helper()
	return evaluateWithInput(t, stdout, stderr, nil, alter)
}

func evaluateWithInput(t *testing.T, stdout, stderr io.Reader, alterInput func(*predicate.Input), alterSeal func(*task.ProviderExitRecord)) (task.Interpretation, []byte, error) {
	t.Helper()
	seal := validSeal(testTaskID, Predicate().Reference())
	if alterSeal != nil {
		alterSeal(&seal)
	}
	input := predicate.Input{Seal: seal}
	if alterInput != nil {
		alterInput(&input)
	}
	evidence := testEvidence{streams: map[predicate.Stream]io.Reader{predicate.Stdout: stdout, predicate.Stderr: stderr}}
	var answer bytes.Buffer
	interp, err := Predicate().Evaluate(input, evidence, &answer)
	return interp, answer.Bytes(), err
}

func validSeal(taskID string, ref task.PredicateRef) task.ProviderExitRecord {
	raw := []task.RawManifestEntry{
		{Path: "raw/stderr", SHA256: task.ComputeSHA256(nil)},
		{Path: "raw/stdout", SHA256: task.ComputeSHA256(nil)},
	}
	manifest, err := task.MarshalCanonical(raw)
	if err != nil {
		panic(err)
	}
	return task.ProviderExitRecord{SchemaVersion: task.SchemaVersion, RootID: testRootID, TaskID: taskID, SpecSHA256: testSpec, MetaSHA256: testMeta, InvocationState: task.InvocationStarted, Predicate: ref, RawManifest: raw, ManifestSHA256: task.ComputeSHA256(manifest), ClosedAt: "2026-09-13T00:00:00Z"}
}

func sessionFrame(taskID, sessionID string) string {
	return fmt.Sprintf(`{"type":"session","task_id":%q,"session_id":%q}`+"\n", taskID, sessionID)
}

func answerFrame(taskID, text string) string {
	return fmt.Sprintf(`{"type":"answer_chunk","task_id":%q,"text":%q}`+"\n", taskID, text)
}

func resultFrame(taskID, sessionID string, success bool) string {
	return fmt.Sprintf(`{"type":"result","task_id":%q,"session_id":%q,"success":%t}`+"\n", taskID, sessionID, success)
}

type testEvidence struct {
	streams map[predicate.Stream]io.Reader
}

func (e testEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	reader, ok := e.streams[stream]
	if !ok {
		return errors.New("missing test stream")
	}
	return consume(reader)
}

type chunkReader struct {
	data []byte
	size int
	off  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, io.EOF
	}
	n := r.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	copy(p[:n], r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

type failingReader struct {
	data []byte
	done bool
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.done = true
	return n, r.err
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
