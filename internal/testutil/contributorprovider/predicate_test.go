package contributorprovider

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testRootID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestPredicateReferenceBindsContractDigest(t *testing.T) {
	ref := Predicate().Reference()
	if ref != Reference() || ref.Adapter != Provider || ref.Mode != Mode || ref.Version != Version {
		t.Fatalf("reference=%+v", ref)
	}
	if !strings.HasSuffix(Contract(), "\n") || task.ComputeSHA256([]byte(Contract())) != ref.SHA256 {
		t.Fatal("predicate contract digest or terminator changed")
	}
}

func TestPredicateSuccessWithPresentArtifactPreservesExactBytes(t *testing.T) {
	answer := " lead\n☃\x00tail "
	stdout := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: answer})
	evidence := newTestEvidence(stdout, nil, []byte(answer), true)
	input := predicate.Input{Seal: validSeal(t, stdout, nil, []byte(answer), true)}
	var out bytes.Buffer
	interpretation, err := Predicate().Evaluate(input, evidence, &out)
	if err != nil {
		t.Fatal(err)
	}
	if interpretation.Verdict != task.VerdictCommitted || interpretation.Refusal != "" || out.String() != answer {
		t.Fatalf("interpretation=%+v answer=%q", interpretation, out.String())
	}
	wantSession := task.SessionIdentity{Provider: Provider, ConversationID: freshSession(testTaskID)}
	if interpretation.Session == nil || *interpretation.Session != wantSession {
		t.Fatalf("session=%+v", interpretation.Session)
	}
}

func TestPredicateAbsentArtifactFallsBackToEnvelopeAnswer(t *testing.T) {
	answer := "fallback\nanswer"
	stdout := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: answer})
	evidence := newTestEvidence(stdout, nil, nil, true)
	input := predicate.Input{Seal: validSeal(t, stdout, nil, nil, false)}
	var out bytes.Buffer
	interpretation, err := Predicate().Evaluate(input, evidence, &out)
	if err != nil || interpretation.Verdict != task.VerdictCommitted || out.String() != answer {
		t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, out.String(), err)
	}
}

func TestPredicatePresentEmptyOrConflictingArtifactRejects(t *testing.T) {
	answer := "envelope answer"
	stdout := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: answer})
	cases := []struct {
		name     string
		artifact []byte
		want     string
	}{
		{name: "empty", artifact: []byte{}, want: refusalEmptyOutput},
		{name: "conflict", artifact: []byte("different"), want: refusalOutputConflict},
		{name: "oversized conflict", artifact: bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1), want: refusalOutputConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := newTestEvidence(stdout, nil, tc.artifact, true)
			input := predicate.Input{Seal: validSeal(t, stdout, nil, tc.artifact, true)}
			var out bytes.Buffer
			interpretation, err := Predicate().Evaluate(input, evidence, &out)
			if err != nil {
				t.Fatal(err)
			}
			if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != tc.want || out.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q", interpretation, out.String())
			}
		})
	}
}

func TestPredicateRejectsExplicitFailureAndNonzeroExit(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		answer     string
		exitCode   int
		wantPrefix string
	}{
		{name: "explicit rejection", status: "rejected", answer: "permission denied", wantPrefix: "provider-rejected: permission denied"},
		{name: "nonzero exit", status: "complete", answer: "answer", exitCode: 7, wantPrefix: refusalProviderFailure + ": exit_code=7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: tc.status, Answer: tc.answer})
			evidence := newTestEvidence(stdout, nil, []byte(tc.answer), true)
			seal := validSeal(t, stdout, nil, []byte(tc.answer), true)
			seal.ExitCode = tc.exitCode
			var out bytes.Buffer
			interpretation, err := Predicate().Evaluate(predicate.Input{Seal: seal}, evidence, &out)
			if err != nil || interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != tc.wantPrefix || out.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, out.String(), err)
			}
		})
	}
}

func TestPredicateMalformedAndIdentityCasesAreSemanticRejections(t *testing.T) {
	cases := []struct {
		name   string
		stdout []byte
		want   string
		input  func(*predicate.Input)
	}{
		{
			name:   "malformed envelope",
			stdout: []byte(`{"protocol":"contributor-proof/v1","task_id":"` + testTaskID + `"`),
			want:   refusalMalformed,
		},
		{
			name:   "oversized envelope",
			stdout: marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: strings.Repeat("x", MaxEnvelopeBytes)}),
			want:   refusalMalformed,
		},
		{
			name:   "wrong task",
			stdout: marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: strings.Repeat("c", 32), SessionID: freshSession(testTaskID), Status: "complete", Answer: "answer"}),
			want:   refusalIdentity,
		},
		{
			name:   "wrong session",
			stdout: marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: "wrong-session", Status: "complete", Answer: "answer"}),
			want:   refusalIdentity,
		},
		{
			name:   "expected continuation mismatch",
			stdout: marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: "answer"}),
			want:   refusalIdentity,
			input: func(input *predicate.Input) {
				input.ExpectedSession = task.SessionExpectation{Required: true, ID: "other-session"}
			},
		},
		{
			name:   "recorded session mismatch",
			stdout: marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: "answer"}),
			want:   refusalIdentity,
			input: func(input *predicate.Input) {
				input.RecordedSession = &task.SessionIdentity{Provider: Provider, ConversationID: "other-session"}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := newTestEvidence(tc.stdout, nil, []byte("answer"), true)
			seal := validSeal(t, tc.stdout, nil, []byte("answer"), true)
			input := predicate.Input{Seal: seal}
			if tc.input != nil {
				tc.input(&input)
			}
			var out bytes.Buffer
			interpretation, err := Predicate().Evaluate(input, evidence, &out)
			if err != nil || interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != tc.want || out.Len() != 0 {
				t.Fatalf("interpretation=%+v answer=%q err=%v", interpretation, out.String(), err)
			}
		})
	}
}

func TestPredicatePropagatesNamedEvidenceFaultsAndSinkFailures(t *testing.T) {
	answer := "answer"
	stdout := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: answer})
	readFailure := errors.New("named evidence read failed")
	evidence := newTestEvidence(stdout, nil, nil, true)
	evidence.namedErr = readFailure
	_, err := Predicate().Evaluate(predicate.Input{Seal: validSeal(t, stdout, nil, nil, false)}, evidence, &bytes.Buffer{})
	if !errors.Is(err, readFailure) {
		t.Fatalf("error=%v, want named evidence fault", err)
	}

	evidence = newTestEvidence(stdout, nil, []byte(answer), true)
	writeFailure := errors.New("answer sink failed")
	_, err = Predicate().Evaluate(predicate.Input{Seal: validSeal(t, stdout, nil, []byte(answer), true)}, evidence, failingWriter{err: writeFailure})
	if !errors.Is(err, writeFailure) {
		t.Fatalf("error=%v, want answer sink failure", err)
	}
}

func TestPredicateRejectsStrictEnvelopeShapeErrors(t *testing.T) {
	validPrefix := `{"protocol":"` + Protocol + `","task_id":"` + testTaskID + `","session_id":"` + freshSession(testTaskID) + `","status":"complete","answer":"answer"}`
	for _, data := range [][]byte{
		[]byte(strings.Replace(validPrefix, `"answer":"answer"}`, `"answer":"answer","extra":1}`, 1)),
		[]byte(strings.Replace(validPrefix, `"answer":"answer"}`, `"answer":"answer"}{}`, 1)),
		[]byte(strings.Replace(validPrefix, `"answer":"answer"}`, `"answer":"answer","answer":"again"}`, 1)),
	} {
		evidence := newTestEvidence(data, nil, []byte("answer"), true)
		seal := validSeal(t, data, nil, []byte("answer"), true)
		var out bytes.Buffer
		interpretation, err := Predicate().Evaluate(predicate.Input{Seal: seal}, evidence, &out)
		if err != nil || interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != refusalMalformed {
			t.Fatalf("interpretation=%+v err=%v", interpretation, err)
		}
	}
}

func TestPredicateRejectsInvalidUTF8BeforeJSONReplacement(t *testing.T) {
	data := marshalEnvelope(t, Envelope{Protocol: Protocol, TaskID: testTaskID, SessionID: freshSession(testTaskID), Status: "complete", Answer: "answer"})
	data = bytes.Replace(data, []byte(`"answer":"answer"`), []byte{'"', 'a', 'n', 's', 'w', 'e', 'r', '"', ':', '"', 0xff, '"'}, 1)
	evidence := newTestEvidence(data, nil, nil, true)
	var out bytes.Buffer
	interpretation, err := Predicate().Evaluate(predicate.Input{Seal: validSeal(t, data, nil, nil, false)}, evidence, &out)
	if err != nil || interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != refusalMalformed || out.Len() != 0 {
		t.Fatalf("invalid UTF-8 was repaired into an answer: %+v output=%q error=%v", interpretation, out.String(), err)
	}
}

type testEvidence struct {
	stdout, stderr []byte
	named          []byte
	namedDeclared  bool
	namedAvailable bool
	namedErr       error
}

func newTestEvidence(stdout, stderr, named []byte, declared bool) *testEvidence {
	return &testEvidence{stdout: stdout, stderr: stderr, named: named, namedDeclared: declared, namedAvailable: named != nil}
}

func (e *testEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	var data []byte
	switch stream {
	case predicate.Stdout:
		data = e.stdout
	case predicate.Stderr:
		data = e.stderr
	default:
		return task.ErrEvidenceFault
	}
	return consume(bytes.NewReader(data))
}

func (e *testEvidence) ReadNamed(name string, consume func(io.Reader) error) error {
	if name != OutputName || !e.namedDeclared {
		return task.ErrEvidenceFault
	}
	if e.namedErr != nil {
		return e.namedErr
	}
	if !e.namedAvailable {
		return predicate.ErrEvidenceAbsent
	}
	return consume(bytes.NewReader(e.named))
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func validSeal(t *testing.T, stdout, stderr, named []byte, namedPresent bool) task.ProviderExitRecord {
	t.Helper()
	manifest := make([]task.RawManifestEntry, 0, 3)
	if namedPresent {
		manifest = append(manifest, task.RawManifestEntry{Path: "raw/" + OutputName, Size: int64(len(named)), SHA256: task.ComputeSHA256(named)})
	}
	manifest = append(manifest,
		task.RawManifestEntry{Path: "raw/stderr", Size: int64(len(stderr)), SHA256: task.ComputeSHA256(stderr)},
		task.RawManifestEntry{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256(stdout)},
	)
	manifestData, err := task.MarshalCanonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return task.ProviderExitRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          testRootID,
		TaskID:          testTaskID,
		SpecSHA256:      strings.Repeat("c", 64),
		MetaSHA256:      strings.Repeat("d", 64),
		InvocationState: task.InvocationStarted,
		Predicate:       Reference(),
		RawManifest:     manifest,
		ManifestSHA256:  task.ComputeSHA256(manifestData),
		ClosedAt:        "2026-09-13T00:00:00Z",
	}
}

func marshalEnvelope(t *testing.T, envelope Envelope) []byte {
	t.Helper()
	data, err := task.MarshalCanonical(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func freshSession(taskID string) string { return "session-" + taskID }

var (
	_ predicate.NamedEvidence = (*testEvidence)(nil)
	_ io.Writer               = failingWriter{}
)
