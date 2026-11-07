package fakeprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRunProviderPreservesBriefAndExpectedStreams(t *testing.T) {
	dir := t.TempDir()
	cfg := testProviderConfig(dir, "success")
	cfg.Answer = "literal ☃ answer"
	cfg.Argv = []string{"$(touch " + filepath.Join(dir, "shell-must-not-run") + "); echo pwned", "literal;token"}
	configPath := writeProviderConfig(t, dir, cfg)
	brief := []byte("brief bytes\x00\nnot parsed")
	var stdout, stderr bytes.Buffer
	if err := RunProvider(configPath, bytes.NewReader(brief), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	assertArtifact(t, filepath.Join(dir, "brief.input"), brief)
	assertArtifact(t, filepath.Join(dir, "expected.stdout"), stdout.Bytes())
	assertArtifact(t, filepath.Join(dir, "expected.stderr"), stderr.Bytes())
	if !bytes.Contains(stdout.Bytes(), []byte("literal ☃ answer")) {
		t.Fatalf("stdout=%q", stdout.Bytes())
	}
	if _, err := os.Stat(filepath.Join(dir, "shell-must-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell-looking argv was executed: %v", err)
	}
	for _, name := range []string{"provider.entry.json", "provider.complete.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var receipt map[string]any
		if err = json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"argv", "process_argv", "cwd", "pid", "entry"} {
			if _, ok := receipt[field]; !ok {
				t.Fatalf("%s missing %s: %s", name, field, data)
			}
		}
	}
}

func TestRunProviderRetainsUniqueReceiptsAcrossDuplicateInvocations(t *testing.T) {
	dir := t.TempDir()
	cfg := testProviderConfig(dir, "success")
	configPath := writeProviderConfig(t, dir, cfg)
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		if err := RunProviderArgs(configPath, cfg.Argv, strings.NewReader("brief"), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("invocation directories=%d, want 2", len(entries))
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("unexpected invocation entry %q", entry.Name())
		}
		entryData, readErr := os.ReadFile(filepath.Join(dir, "invocations", entry.Name(), "provider.entry.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var receipt providerReceipt
		if err = json.Unmarshal(entryData, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.InvocationID != entry.Name() || seen[receipt.InvocationID] {
			t.Fatalf("duplicate or mismatched invocation ID: %+v", receipt)
		}
		seen[receipt.InvocationID] = true
		if _, err = os.Stat(filepath.Join(dir, "invocations", entry.Name(), "provider.complete.json")); err != nil {
			t.Fatalf("missing immutable completion receipt: %v", err)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "provider.entry.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "provider.complete.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRunProviderReportsArgvMismatchAfterWritingEntry(t *testing.T) {
	dir := t.TempDir()
	cfg := testProviderConfig(dir, "success")
	configPath := writeProviderConfig(t, dir, cfg)
	var stdout, stderr bytes.Buffer
	if err := RunProviderArgs(configPath, []string{"different"}, strings.NewReader("brief"), &stdout, &stderr); err == nil {
		t.Fatal("argv mismatch accepted")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry was not retained for mismatch: %d", len(entries))
	}
	if _, err = os.Stat(filepath.Join(dir, "invocations", entries[0].Name(), "provider.entry.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledParentExitTracksChildUntilNaturalClose(t *testing.T) {
	dir := t.TempDir()
	binary := buildCompiledProvider(t, dir)
	cfg := testProviderConfig(dir, "parent_exit")
	// Keep the child rendezvous open long enough for this compiled-process
	// assertion to remain reliable when the package runs beside the race suite.
	cfg.ChildLifetimeMS = 10_000
	cfg.RendezvousPath = filepath.Join(dir, "child-rendezvous")
	cfg.Argv = []string{"$(literal)", "value;token"}
	configPath := writeProviderConfig(t, dir, cfg)
	process := startCompiledProcess(t, binary, configPath, cfg.Argv)

	if err := waitForFixtureFile(cfg.RendezvousPath+".waiting", 5*time.Second); err != nil {
		// The child lifetime is bounded and the release below lets it finish
		// naturally even when this assertion fails.
		writeFixtureRelease(t, cfg.RendezvousPath)
		process.awaitNatural()
		t.Fatal(err)
	}
	parentErr := process.awaitParent(2 * time.Second)
	if parentErr != nil {
		writeFixtureRelease(t, cfg.RendezvousPath)
		process.awaitStreams()
		t.Fatalf("parent provider: %v", parentErr)
	}

	answerBeforeRelease := process.answerBeforeRelease(20 * time.Millisecond)
	output := releaseCompiledProcess(t, process, cfg.RendezvousPath)
	assertCompiledChildOutput(t, output, answerBeforeRelease)
	assertLiteralArgvWasInert(t, dir, cfg.Argv)

	parent, child := readParentChildReceipts(t, filepath.Join(dir, "invocations"))
	assertParentChildReceipts(t, parent, child, cfg.Argv)
}

func releaseCompiledProcess(t *testing.T, process compiledProcess, rendezvous string) compiledOutput {
	t.Helper()
	if err := os.WriteFile(rendezvous+".release", []byte("release\n"), 0o600); err != nil {
		process.awaitStreams()
		t.Fatal(err)
	}
	output := process.awaitOutput(2 * time.Second)
	if output.err != nil {
		t.Fatal(output.err)
	}
	if err := process.awaitStderr(); err != nil {
		t.Fatal(err)
	}
	return output
}

func assertCompiledChildOutput(t *testing.T, output compiledOutput, answerBeforeRelease bool) {
	t.Helper()
	if !bytes.Contains(output.data, []byte(`"type":"session"`)) || !bytes.Contains(output.data, []byte(`"type":"answer_chunk"`)) || !bytes.Contains(output.data, []byte(`"type":"result"`)) {
		t.Fatalf("compiled child output=%q", output.data)
	}
	if answerBeforeRelease {
		t.Fatalf("child answer arrived before rendezvous release: %q", output.data)
	}
}

func assertLiteralArgvWasInert(t *testing.T, dir string, argv []string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "shell-must-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell-looking argv %q was executed: %v", argv, err)
	}
}

func assertParentChildReceipts(t *testing.T, parent, child providerReceipt, argv []string) {
	t.Helper()
	if parent.ChildPID <= 0 || child.PID != parent.ChildPID || !parent.Natural || !child.Natural {
		t.Fatalf("parent=%+v child=%+v", parent, child)
	}
	parentCompleted, err := time.Parse(time.RFC3339Nano, parent.Completed)
	if err != nil {
		t.Fatal(err)
	}
	childCompleted, err := time.Parse(time.RFC3339Nano, child.Completed)
	if err != nil {
		t.Fatal(err)
	}
	if !parentCompleted.Before(childCompleted) {
		t.Fatalf("parent completion %s did not precede child completion %s", parent.Completed, child.Completed)
	}
	if len(parent.Argv) != len(argv) || !equalStrings(parent.Argv, argv) {
		t.Fatalf("parent literal argv=%q want=%q", parent.Argv, argv)
	}
}

func buildCompiledProvider(t *testing.T, dir string) string {
	t.Helper()
	binary := filepath.Join(dir, "provider")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/provider")
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build.Dir = packageDir
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("building compiled provider: %v\n%s", err, buildOutput)
	}
	return binary
}

type compiledProcess struct {
	stdoutDone chan compiledOutput
	stderrDone chan error
	parentDone chan error
	answerSeen chan struct{}
}

func startCompiledProcess(t *testing.T, binary, configPath string, argv []string) compiledProcess {
	t.Helper()
	cmd := exec.Command(binary, append([]string{configPath}, argv...)...)
	cmd.Stdin = strings.NewReader("brief")
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		closeTestFiles(t, stdoutReader, stdoutWriter)
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	if err = cmd.Start(); err != nil {
		closeTestFiles(t, stdoutReader, stdoutWriter, stderrReader, stderrWriter)
		t.Fatal(err)
	}
	closeTestFiles(t, stdoutWriter, stderrWriter)

	answerSeen := make(chan struct{})
	stdoutDone := make(chan compiledOutput, 1)
	go func() { stdoutDone <- readCompiledOutput(stdoutReader, answerSeen) }()
	stderrDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(io.Discard, stderrReader)
		stderrDone <- errors.Join(readErr, stderrReader.Close())
	}()
	parentDone := make(chan error, 1)
	go func() { parentDone <- cmd.Wait() }()
	return compiledProcess{stdoutDone: stdoutDone, stderrDone: stderrDone, parentDone: parentDone, answerSeen: answerSeen}
}

func closeTestFiles(t *testing.T, files ...*os.File) {
	t.Helper()
	for _, file := range files {
		if err := file.Close(); err != nil {
			t.Logf("closing test pipe: %v", err)
		}
	}
}

func (p compiledProcess) awaitParent(timeout time.Duration) error {
	return awaitCompiledProcess(p.parentDone, timeout)
}

func (p compiledProcess) awaitOutput(timeout time.Duration) compiledOutput {
	return awaitCompiledOutput(p.stdoutDone, timeout)
}

func (p compiledProcess) awaitStderr() error { return <-p.stderrDone }

func (p compiledProcess) awaitNatural() {
	<-p.parentDone
	p.awaitStreams()
}

func (p compiledProcess) awaitStreams() {
	<-p.stdoutDone
	<-p.stderrDone
}

func (p compiledProcess) answerBeforeRelease(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.answerSeen:
		return true
	case <-timer.C:
		return false
	}
}

func writeFixtureRelease(t *testing.T, rendezvous string) {
	t.Helper()
	if err := os.WriteFile(rendezvous+".release", []byte("release\n"), 0o600); err != nil {
		t.Logf("writing fixture release: %v", err)
	}
}

func TestRunProviderLateSessionHasFiniteObservableGap(t *testing.T) {
	dir := t.TempDir()
	cfg := testProviderConfig(dir, "late_session")
	cfg.LifetimeMS = 80
	configPath := writeProviderConfig(t, dir, cfg)
	synctest.Test(t, func(t *testing.T) {
		writer := &timedProviderWriter{}
		if err := RunProviderArgs(configPath, cfg.Argv, strings.NewReader("brief"), writer, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		events := decodeProviderEvents(t, writer.data.Bytes())
		assertProviderEventTypes(t, events, "answer_chunk", "session", "result")
		if events[0].Text != "late session answer" || !events[2].Success {
			t.Fatalf("late-session events=%+v", events)
		}
		if len(writer.writes) != len(events) {
			t.Fatalf("fixture frame writes=%d, want %d", len(writer.writes), len(events))
		}
		// Observe the gap at the writer itself. The observer cannot miss it
		// because it was scheduled after the session frame arrived.
		if gap := writer.writes[1].Sub(writer.writes[0]); gap != 80*time.Millisecond {
			t.Fatalf("answer-to-session gap=%s, want 80ms", gap)
		}
	})
}

func TestRunProviderSplitsLongLiteralAnswerIntoBoundedFrames(t *testing.T) {
	for _, answer := range []string{strings.Repeat("☃", 40*1024), strings.Repeat("\x00", 20*1024)} {
		dir := t.TempDir()
		cfg := testProviderConfig(dir, "success")
		cfg.Answer = answer
		configPath := writeProviderConfig(t, dir, cfg)
		var stdout, stderr bytes.Buffer
		if err := RunProvider(configPath, strings.NewReader("brief"), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		assertProviderAnswer(t, stdout.Bytes(), cfg, answer)
	}
}

type providerScenarioCase struct {
	name       string
	types      []string
	answer     string
	stderrWant string
	newline    bool
	exitCode   int
}

func TestRunProviderScenariosAndFiniteLargeStreams(t *testing.T) {
	cases := []providerScenarioCase{
		{name: "empty", types: []string{"session", "answer_chunk", "result"}, answer: " \t\n", newline: true},
		{name: "diagnostic", types: []string{"session", "answer_chunk", "result"}, answer: "brief", stderrWant: stderrMarker, newline: true},
		{name: "malformed", types: []string{"session", "unknown"}, newline: true},
		{name: "truncated", types: []string{"session", "answer_chunk", "result"}, answer: "truncated"},
		{name: "late_session", types: []string{"answer_chunk", "session", "result"}, answer: "late session answer", newline: true},
		{name: "nonzero", types: []string{"session", "answer_chunk", "result"}, answer: "brief", newline: true, exitCode: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runProviderScenarioCase(t, tc) })
	}

	dir := t.TempDir()
	cfg := testProviderConfig(dir, "large")
	cfg.StdoutBytes, cfg.StderrBytes = (1<<20)+17, (1<<20)+29
	configPath := writeProviderConfig(t, dir, cfg)
	var stdout, stderr bytes.Buffer
	if err := RunProvider(configPath, strings.NewReader("brief"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	assertProviderAnswer(t, stdout.Bytes(), cfg, strings.Repeat("S", int(cfg.StdoutBytes)))
	if !bytes.Equal(stderr.Bytes(), bytes.Repeat([]byte{'E'}, int(cfg.StderrBytes))) {
		t.Fatalf("large stderr did not preserve its configured %d bytes", cfg.StderrBytes)
	}
	assertArtifact(t, filepath.Join(dir, "expected.stdout"), stdout.Bytes())
	assertArtifact(t, filepath.Join(dir, "expected.stderr"), stderr.Bytes())
}

func runProviderScenarioCase(t *testing.T, tc providerScenarioCase) {
	t.Helper()
	dir := t.TempDir()
	cfg := testProviderConfig(dir, tc.name)
	configPath := writeProviderConfig(t, dir, cfg)
	var stdout, stderr bytes.Buffer
	err := RunProvider(configPath, strings.NewReader("brief"), &stdout, &stderr)
	if tc.exitCode == 0 {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var exitErr *ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != tc.exitCode {
			t.Fatalf("error=%v", err)
		}
	}
	assertProviderScenarioOutput(t, tc, cfg, stdout.Bytes(), stderr.Bytes())
	assertArtifact(t, filepath.Join(dir, "expected.stdout"), stdout.Bytes())
	assertArtifact(t, filepath.Join(dir, "expected.stderr"), stderr.Bytes())
}

func assertProviderScenarioOutput(t *testing.T, tc providerScenarioCase, cfg ProviderConfig, stdout, stderr []byte) {
	t.Helper()
	events := decodeProviderEvents(t, stdout)
	assertProviderEventTypes(t, events, tc.types...)
	var answer strings.Builder
	for _, event := range events {
		if event.TaskID != cfg.TaskID || event.Type == "session" && event.SessionID != cfg.SessionID || event.Type == "result" && (event.SessionID != cfg.SessionID || !event.Success) {
			t.Fatalf("unexpected fixture identity or result: %+v", event)
		}
		if event.Type == "answer_chunk" {
			answer.WriteString(event.Text)
		}
	}
	if answer.String() != tc.answer || string(stderr) != tc.stderrWant || bytes.HasSuffix(stdout, []byte{'\n'}) != tc.newline {
		t.Fatalf("scenario=%s answer=%q stderr=%q newline=%t", tc.name, answer.String(), string(stderr), bytes.HasSuffix(stdout, []byte{'\n'}))
	}
}

// This schema is the fixture's documented wire contract, independently of its encoder.
type providerEvent struct {
	Type      string `json:"type"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	Success   bool   `json:"success"`
}

func decodeProviderEvents(t *testing.T, output []byte) []providerEvent {
	t.Helper()
	frames := bytes.Split(bytes.TrimSuffix(output, []byte{'\n'}), []byte{'\n'})
	events := make([]providerEvent, 0, len(frames))
	for _, frame := range frames {
		if len(frame) == 0 || len(frame) > maxFrameBytes {
			t.Fatalf("frame length=%d, want 1..%d", len(frame), maxFrameBytes)
		}
		var event providerEvent
		if err := json.Unmarshal(frame, &event); err != nil {
			t.Fatalf("invalid fixture event: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func assertProviderEventTypes(t *testing.T, events []providerEvent, types ...string) {
	t.Helper()
	if len(events) != len(types) {
		t.Fatalf("fixture event count=%d, want %d", len(events), len(types))
	}
	for index, kind := range types {
		if events[index].Type != kind {
			t.Fatalf("fixture event %d=%s, want %s", index, events[index].Type, kind)
		}
	}
}

func assertProviderAnswer(t *testing.T, output []byte, cfg ProviderConfig, expected string) {
	t.Helper()
	events := decodeProviderEvents(t, output)
	if len(events) < 3 || events[0].Type != "session" || events[0].SessionID != cfg.SessionID || events[len(events)-1].Type != "result" || !events[len(events)-1].Success || events[len(events)-1].SessionID != cfg.SessionID || !bytes.HasSuffix(output, []byte{'\n'}) {
		t.Fatal("fixture omitted its session, answer, or successful terminal frame")
	}
	var answer strings.Builder
	for index, event := range events {
		if event.TaskID != cfg.TaskID {
			t.Fatalf("fixture event %d has foreign task identity", index)
		}
		if index > 0 && index < len(events)-1 {
			if event.Type != "answer_chunk" {
				t.Fatalf("fixture answer frame %d has type %s", index, event.Type)
			}
			answer.WriteString(event.Text)
		}
	}
	if answer.String() != expected {
		t.Fatalf("fixture answer differs: got %d bytes, want %d", answer.Len(), len(expected))
	}
}

func TestRunProviderRejectsInvalidConfigAndUsesFiniteHold(t *testing.T) {
	dir := t.TempDir()
	valid := testProviderConfig(dir, "success")
	data, err := task.MarshalCanonical(valid)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := strings.TrimSpace(string(data))
	jsonText = strings.TrimSuffix(jsonText, "}") + `,"Scenario":"success"}` + "\n"
	data = []byte(jsonText)
	path := filepath.Join(dir, "bad.json")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = RunProvider(path, strings.NewReader("brief"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("case-variant config field accepted")
	}

	cfg := valid
	cfg.Scenario, cfg.LifetimeMS = "hold", 5
	path = writeProviderConfig(t, dir, cfg)
	start := time.Now()
	if err = RunProvider(path, strings.NewReader("brief"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("finite hold duration=%s", elapsed)
	}
}

func testProviderConfig(dir, scenario string) ProviderConfig {
	return ProviderConfig{Scenario: scenario, ArtifactDir: filepath.Clean(dir), LifetimeMS: 100, TaskID: testTaskID, SessionID: "conv-1", Argv: []string{"--literal", "value"}}
}

func writeProviderConfig(t *testing.T, dir string, cfg ProviderConfig) string {
	t.Helper()
	data, err := task.MarshalCanonical(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider-fixture.json")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertArtifact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("artifact %s differs: got=%d want=%d", path, len(got), len(want))
	}
}

type timedProviderWriter struct {
	data   bytes.Buffer
	writes []time.Time
}

func (w *timedProviderWriter) Write(data []byte) (int, error) {
	w.writes = append(w.writes, time.Now())
	return w.data.Write(data)
}

type compiledOutput struct {
	data []byte
	err  error
}

func readCompiledOutput(reader *os.File, answerSeen chan<- struct{}) compiledOutput {
	var data bytes.Buffer
	buf := make([]byte, 4096)
	answerReported := false
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			_, _ = data.Write(buf[:n])
			if !answerReported && bytes.Contains(data.Bytes(), []byte(`"type":"answer_chunk"`)) {
				close(answerSeen)
				answerReported = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return compiledOutput{data: data.Bytes(), err: errors.Join(err, reader.Close())}
		}
	}
}

func awaitCompiledProcess(done <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("compiled provider did not complete within %s", timeout)
	}
}

func awaitCompiledOutput(done <-chan compiledOutput, timeout time.Duration) compiledOutput {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case output := <-done:
		return output
	case <-timer.C:
		return compiledOutput{err: fmt.Errorf("compiled child stream did not close within %s", timeout)}
	}
}

func waitForFixtureFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("fixture file %s did not appear within %s", path, timeout)
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
			return fmt.Errorf("fixture file %s did not appear within %s", path, timeout)
		case <-time.After(5 * time.Millisecond):
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func readParentChildReceipts(t *testing.T, invocations string) (providerReceipt, providerReceipt) {
	t.Helper()
	entries, err := os.ReadDir(invocations)
	if err != nil {
		t.Fatal(err)
	}
	var parent, child providerReceipt
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(invocations, entry.Name(), "provider.complete.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var receipt providerReceipt
		if err = json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		switch receipt.Scenario {
		case "parent_exit":
			parent = receipt
		case "child":
			child = receipt
		}
	}
	if parent.InvocationID == "" || child.InvocationID == "" {
		t.Fatalf("missing parent/child completion receipts in %s", invocations)
	}
	return parent, child
}
