package fixture

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

type runFixture struct {
	runtimeDir    string
	runtimeConfig string
	toolsConfig   string
	output        string
	taskID        string
	sessionID     string
}

func newRunFixture(t *testing.T, taskID string) runFixture {
	t.Helper()
	runtimeDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeConfig := filepath.Join(runtimeDir, protocol.RuntimeInputName)
	toolsConfig := filepath.Join(runtimeDir, protocol.ToolsInputName)
	fixture := runFixture{
		runtimeDir:    runtimeDir,
		runtimeConfig: runtimeConfig,
		toolsConfig:   toolsConfig,
		output:        filepath.Join(runtimeDir, protocol.OutputName),
		taskID:        taskID,
		sessionID:     "session-" + taskID,
	}
	if err := writeJSONOnce(runtimeConfig, protocol.RuntimeConfig{SchemaVersion: protocol.SchemaVersion, RuntimeDir: runtimeDir}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONOnce(toolsConfig, protocol.ToolsConfig{SchemaVersion: protocol.SchemaVersion, AllowedTools: []string{}, WorkspaceWrite: false}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func runArguments(fixture runFixture, resume bool) []string {
	args := []string{
		"--runtime-config", fixture.runtimeConfig,
		"--tools-config", fixture.toolsConfig,
		"--output", fixture.output,
		"--task-id", fixture.taskID,
		"--session-id", fixture.sessionID,
	}
	if resume {
		args = append(args, "--resume")
	}
	return args
}

func marshalBrief(t *testing.T, brief protocol.Brief) []byte {
	t.Helper()
	data, err := task.MarshalCanonical(brief)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runFixtureOnce(t *testing.T, fixture runFixture, brief protocol.Brief, resume bool) (int, bytes.Buffer, bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(runArguments(fixture, resume), bytes.NewReader(marshalBrief(t, brief)), &stdout, &stderr)
	return code, stdout, stderr
}

func decodeEnvelope(t *testing.T, data []byte) protocol.Envelope {
	t.Helper()
	var envelope protocol.Envelope
	if err := task.DecodeStrict(data, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func readFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readReceipt(t *testing.T, path string) executionReceipt {
	t.Helper()
	return decodeReceipt(t, readFixtureFile(t, path))
}

func decodeReceipt(t *testing.T, data []byte) executionReceipt {
	t.Helper()
	var receipt executionReceipt
	if err := task.DecodeStrict(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, got %v", path, err)
	}
}

func TestRunReadsBothConfigsAndResumesExactNonce(t *testing.T) {
	first := newRunFixture(t, strings.Repeat("a", 32))
	firstBrief := protocol.Brief{Case: protocol.CasePresent, Answer: "fresh answer", Nonce: "remembered nonce ☃"}
	assertFreshRun(t, first, firstBrief)

	resume := first
	resume.taskID = strings.Repeat("b", 32)
	resume.output = filepath.Join(first.runtimeDir, "resume.txt")
	assertResumeRun(t, first, resume, firstBrief.Nonce)
}

func assertFreshRun(t *testing.T, fixture runFixture, brief protocol.Brief) {
	t.Helper()
	code, stdout, stderr := runFixtureOnce(t, fixture, brief, false)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("fresh run code=%d stderr=%q", code, stderr.String())
	}
	envelope := decodeEnvelope(t, stdout.Bytes())
	if envelope.Protocol != protocol.Protocol || envelope.TaskID != fixture.taskID || envelope.SessionID != fixture.sessionID || envelope.Status != "complete" || envelope.Answer != brief.Answer {
		t.Fatalf("fresh envelope=%+v", envelope)
	}
	if got := readFixtureFile(t, fixture.output); string(got) != brief.Answer {
		t.Fatalf("fresh artifact=%q", got)
	}
	assertFreshSession(t, fixture, brief)
	assertFreshReceipts(t, fixture)
}

func assertFreshSession(t *testing.T, fixture runFixture, brief protocol.Brief) {
	t.Helper()
	session := readFixtureFile(t, filepath.Join(fixture.runtimeDir, "sessions", fixture.sessionID+".json"))
	var recorded sessionRecord
	if err := task.DecodeStrict(session, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.SessionID != fixture.sessionID || recorded.Nonce != brief.Nonce {
		t.Fatalf("recorded session=%+v", recorded)
	}
}

func assertFreshReceipts(t *testing.T, fixture runFixture) {
	t.Helper()
	launch := readReceipt(t, filepath.Join(fixture.runtimeDir, "launches", fixture.taskID+".json"))
	completion := readReceipt(t, filepath.Join(fixture.runtimeDir, "completions", fixture.taskID+".json"))
	if launch.Status != "entered" || completion.Status != "completed" || launch.TaskID != fixture.taskID || completion.SessionID != fixture.sessionID {
		t.Fatalf("receipts launch=%+v completion=%+v", launch, completion)
	}
}

func assertResumeRun(t *testing.T, first, resume runFixture, nonce string) {
	t.Helper()
	resumeBrief := protocol.Brief{Case: protocol.CaseResume}
	briefBytes := marshalBrief(t, resumeBrief)
	if bytes.Contains(briefBytes, []byte(`"answer"`)) || bytes.Contains(briefBytes, []byte(`"nonce"`)) {
		t.Fatalf("resume brief supplied remembered fields: %s", briefBytes)
	}
	var resumeStdout, resumeStderr bytes.Buffer
	code := Run(runArguments(resume, true), bytes.NewReader(briefBytes), &resumeStdout, &resumeStderr)
	if code != 0 || resumeStderr.Len() != 0 {
		t.Fatalf("resume run code=%d stderr=%q", code, resumeStderr.String())
	}
	resumeEnvelope := decodeEnvelope(t, resumeStdout.Bytes())
	if resumeEnvelope.TaskID != resume.taskID || resumeEnvelope.SessionID != resume.sessionID || resumeEnvelope.Answer != nonce || resumeEnvelope.Status != "complete" {
		t.Fatalf("resume envelope=%+v", resumeEnvelope)
	}
	if got := readFixtureFile(t, resume.output); string(got) != nonce {
		t.Fatalf("resume artifact=%q", got)
	}
	resumeCompletion := readReceipt(t, filepath.Join(first.runtimeDir, "completions", resume.taskID+".json"))
	if resumeCompletion.TaskID != resume.taskID || resumeCompletion.SessionID != first.sessionID || resumeCompletion.Status != "completed" {
		t.Fatalf("resume completion=%+v", resumeCompletion)
	}
}

func TestRunOptionalOutputCasesAndNonzeroExit(t *testing.T) {
	cases := []struct {
		name       string
		caseName   string
		wantCode   int
		present    bool
		wantOutput []byte
	}{
		{name: "present", caseName: protocol.CasePresent, wantCode: 0, present: true, wantOutput: []byte("answer")},
		{name: "absent", caseName: protocol.CaseAbsent, wantCode: 0, wantOutput: nil},
		{name: "empty", caseName: protocol.CaseEmpty, wantCode: 0, present: true, wantOutput: []byte{}},
		{name: "conflict", caseName: protocol.CaseConflict, wantCode: 0, present: true, wantOutput: []byte("answer conflicting artifact")},
		{name: "nonzero", caseName: protocol.CaseNonzero, wantCode: 7, present: true, wantOutput: []byte("answer")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunFixture(t, strings.Repeat("c", 32))
			brief := protocol.Brief{Case: test.caseName, Answer: "answer", Nonce: "case nonce"}
			code, stdout, stderr := runFixtureOnce(t, fixture, brief, false)
			if code != test.wantCode || stderr.Len() != 0 {
				t.Fatalf("case=%s code=%d stderr=%q", test.caseName, code, stderr.String())
			}
			envelope := decodeEnvelope(t, stdout.Bytes())
			if envelope.TaskID != fixture.taskID || envelope.SessionID != fixture.sessionID || envelope.Status != "complete" || envelope.Answer != brief.Answer {
				t.Fatalf("case=%s envelope=%+v", test.caseName, envelope)
			}
			outputPath := fixture.output
			if test.present {
				if got := readFixtureFile(t, outputPath); !bytes.Equal(got, test.wantOutput) {
					t.Fatalf("case=%s output=%q want=%q", test.caseName, got, test.wantOutput)
				}
			} else {
				assertAbsent(t, outputPath)
			}
		})
	}
}

func TestRunRefusesDuplicateTaskCreateOnce(t *testing.T) {
	fixture := newRunFixture(t, strings.Repeat("d", 32))
	brief := protocol.Brief{Case: protocol.CasePresent, Answer: "answer", Nonce: "nonce"}
	code, stdout, stderr := runFixtureOnce(t, fixture, brief, false)
	if code != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
		t.Fatalf("first run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	launchPath := filepath.Join(fixture.runtimeDir, "launches", fixture.taskID+".json")
	completionPath := filepath.Join(fixture.runtimeDir, "completions", fixture.taskID+".json")
	launchBefore := readFixtureFile(t, launchPath)
	completionBefore := readFixtureFile(t, completionPath)

	code, duplicateStdout, duplicateStderr := runFixtureOnce(t, fixture, brief, false)
	if code != 2 || duplicateStdout.Len() != 0 || duplicateStderr.Len() == 0 {
		t.Fatalf("duplicate run code=%d stdout=%q stderr=%q", code, duplicateStdout.String(), duplicateStderr.String())
	}
	if launchAfter := readFixtureFile(t, launchPath); !bytes.Equal(launchBefore, launchAfter) {
		t.Fatal("duplicate run replaced launch receipt")
	}
	if completionAfter := readFixtureFile(t, completionPath); !bytes.Equal(completionBefore, completionAfter) {
		t.Fatal("duplicate run replaced completion receipt")
	}
}

func TestRunRejectsMalformedRuntimeOrToolsConfig(t *testing.T) {
	for _, name := range []string{"runtime", "tools"} {
		t.Run(name, func(t *testing.T) {
			fixture := newRunFixture(t, strings.Repeat("e", 32))
			path := fixture.runtimeConfig
			if name == "tools" {
				path = fixture.toolsConfig
			}
			if err := os.WriteFile(path, []byte("{malformed"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runFixtureOnce(t, fixture, protocol.Brief{Case: protocol.CasePresent, Answer: "answer", Nonce: "nonce"}, false)
			if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("malformed %s config code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
			}
			assertAbsent(t, filepath.Join(fixture.runtimeDir, "launches", fixture.taskID+".json"))
		})
	}
}

type failingRunWriter struct{ err error }

func (w failingRunWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunPropagatesStdoutWriterFault(t *testing.T) {
	fixture := newRunFixture(t, strings.Repeat("f", 32))
	brief := marshalBrief(t, protocol.Brief{Case: protocol.CasePresent, Answer: "answer", Nonce: "nonce"})
	writeErr := errors.New("injected stdout writer failure")
	var stderr bytes.Buffer
	code := Run(runArguments(fixture, false), bytes.NewReader(brief), failingRunWriter{err: writeErr}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), writeErr.Error()) {
		t.Fatalf("stdout fault code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(fixture.output); err != nil {
		t.Fatalf("output was not written before stdout fault: %v", err)
	}
	assertAbsent(t, filepath.Join(fixture.runtimeDir, "completions", fixture.taskID+".json"))
}

var _ io.Writer = failingRunWriter{}
