package phase2supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

type testHarness struct {
	executable     string
	config         string
	expected       string
	artifact       string
	status         string
	cleanupPending *pueue.Pending
}

func newTestHarness(t *testing.T) testHarness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rawExecutable := filepath.Join(base, "pueue fake")
	err = os.WriteFile(rawExecutable, []byte("compiled fake"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks(rawExecutable)
	if err != nil {
		t.Fatal(err)
	}
	h := testHarness{
		executable: executable,
		config:     filepath.Join(filepath.Dir(executable), ConfigName),
		artifact:   filepath.Join(base, "receipts"),
		status:     filepath.Join(base, "status.json"),
	}
	if err := os.WriteFile(h.status, []byte(`{"tasks":{},"groups":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(h.config, h.artifact, h.status)
	if err := WriteConfig(h.config, cfg); err != nil {
		t.Fatal(err)
	}
	return h
}

func newCompiledHarness(t *testing.T) *testHarness {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dls-")
	if err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(base, "pueue")
	h := &testHarness{
		executable: executable,
		config:     filepath.Join(base, ConfigName),
		expected:   filepath.Join(base, "pueue.yml"),
		artifact:   filepath.Join(base, "receipts"),
		status:     filepath.Join(base, "status.json"),
	}
	t.Cleanup(func() {
		if h.cleanupPending != nil && !waitPendingNaturally(h.cleanupPending, 15*time.Second) {
			t.Logf("preserving compiled supervisor files while Bind remains in flight: %s", base)
			return
		}
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	buildCompiledFake(t, executable)
	if err := os.WriteFile(h.status, []byte(`{"tasks":{},"groups":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.expected, productionConfig(t, base), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(h.config, h.artifact, h.status)
	cfg.ExpectedConfigPath = h.expected
	if err := WriteConfig(h.config, cfg); err != nil {
		t.Fatal(err)
	}
	return h
}

func waitPendingNaturally(pending *pueue.Pending, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-pending.Done():
		return true
	case <-timer.C:
		return false
	}
}

func bindCompiledHarness(t *testing.T, h *testHarness) *pueue.Client {
	t.Helper()
	client, err := pueue.Bind(context.Background(), h.executable, h.expected, pueue.Options{ObservationTimeout: pueue.DefaultObservationTimeout})
	if err == nil {
		return client
	}
	var inFlight *pueue.InFlightError
	if errors.As(err, &inFlight) && inFlight.Pending != nil {
		h.cleanupPending = inFlight.Pending
		if !waitPendingNaturally(inFlight.Pending, 15*time.Second) {
			t.Fatalf("compiled supervisor binding remained in flight: %v", err)
		}
		h.cleanupPending = nil
	}
	t.Fatal(err)
	return nil
}

func buildCompiledFake(t *testing.T, output string) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating phase2supervisor source")
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(source))))
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool, "build", "-buildvcs=false", "-o", output, "./internal/testutil/phase2supervisor/cmd/pueue")
	cmd.Dir = repo
	if outputData, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build compiled phase2 supervisor: %v: %s", err, outputData)
	}
}

func productionConfig(t *testing.T, base string) []byte {
	t.Helper()
	shared := map[string]string{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "run"),
		"unix_socket_path":   filepath.Join(base, "socket"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}
	data, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func captureRun(t *testing.T, run func() int) (int, []byte, []byte) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errRead, errWrite, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	os.Stdout, os.Stderr = outWrite, errWrite
	code := run()
	err = outWrite.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = errWrite.Close()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	stdout, err := io.ReadAll(outRead)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(errRead)
	if err != nil {
		t.Fatal(err)
	}
	if err := outRead.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errRead.Close(); err != nil {
		t.Fatal(err)
	}
	return code, stdout, stderr
}

func readTestReceipts(t *testing.T, artifact string) (EntryReceipt, CompletionReceipt) {
	t.Helper()
	entries, err := os.ReadDir(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var entry EntryReceipt
	var completion CompletionReceipt
	for _, dirEntry := range entries {
		if strings.HasSuffix(dirEntry.Name(), ".tmp") {
			t.Fatalf("temporary receipt remained: %s", dirEntry.Name())
		}
		data, err := os.ReadFile(filepath.Join(artifact, dirEntry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(dirEntry.Name(), ".entry.json"):
			if err := json.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(dirEntry.Name(), ".completion.json"):
			if err := json.Unmarshal(data, &completion); err != nil {
				t.Fatal(err)
			}
		}
	}
	if entry.InvocationID == "" || completion.InvocationID == "" || entry.InvocationID != completion.InvocationID {
		t.Fatalf("incomplete receipt pair: %+v %+v", entry, completion)
	}
	return entry, completion
}

func readReceiptForArgs(t *testing.T, artifact string, argv []string) (EntryReceipt, CompletionReceipt) {
	t.Helper()
	entries, err := os.ReadDir(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, dirEntry := range entries {
		if !strings.HasSuffix(dirEntry.Name(), ".entry.json") {
			continue
		}
		entryData, err := os.ReadFile(filepath.Join(artifact, dirEntry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var entry EntryReceipt
		err = json.Unmarshal(entryData, &entry)
		if err != nil {
			t.Fatal(err)
		}
		if !equalStrings(entry.Argv, argv) {
			continue
		}
		completionData, err := os.ReadFile(filepath.Join(artifact, entry.InvocationID+".completion.json"))
		if err != nil {
			t.Fatal(err)
		}
		var completion CompletionReceipt
		if err := json.Unmarshal(completionData, &completion); err != nil {
			t.Fatal(err)
		}
		return entry, completion
	}
	t.Fatalf("receipt not found for argv %q", argv)
	return EntryReceipt{}, CompletionReceipt{}
}

func TestVersionInvocationHasCompleteReceipt(t *testing.T) {
	h := newTestHarness(t)
	versionArgs := []string{"-c", h.config, "--version"}
	code, stdout, stderr := captureRun(t, func() int {
		return RunWithExecutable(h.executable, h.config, versionArgs)
	})
	if code != 0 || !bytes.Equal(stdout, []byte("pueue 4.0.4\n")) || len(stderr) != 0 {
		t.Fatalf("version result code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	entry, completion := readTestReceipts(t, h.artifact)
	if entry.ConfigPath != h.config || !equalStrings(entry.Argv, versionArgs) || entry.Verb != "--version" || completion.ExitCode != 0 {
		t.Fatalf("version receipt lost literal invocation: %+v %+v", entry, completion)
	}
	if completion.PID != entry.PID || completion.ConfigPath != h.config || completion.StdoutBytes != int64(len(stdout)) || completion.StdoutSHA256 != hashOutput(stdout) {
		t.Fatalf("version completion receipt mismatch: %+v", completion)
	}
}

func TestInvalidInvocationHasCompleteReceipt(t *testing.T) {
	h := newTestHarness(t)
	invalidArgs := []string{"-c", h.config, "kill", "--all"}
	code, _, stderr := captureRun(t, func() int {
		return RunWithExecutable(h.executable, h.config, invalidArgs)
	})
	if code != 64 || !bytes.Contains(stderr, []byte(ErrInvalidArguments.Error())) {
		t.Fatalf("invalid invocation result code=%d stderr=%q", code, stderr)
	}
	entry, completion := readReceiptForArgs(t, h.artifact, invalidArgs)
	if !equalStrings(entry.Argv, invalidArgs) || completion.ExitCode != 64 || completion.Verb != "kill" {
		t.Fatalf("invalid invocation was not fully recorded: %+v %+v", entry, completion)
	}
}

func TestAddPreservesLiteralOutputAndArguments(t *testing.T) {
	h := newTestHarness(t)
	config, err := LoadConfig(h.config)
	if err != nil {
		t.Fatal(err)
	}
	config.AddID = 42
	config.Add.ExitCode = 0
	if err := WriteConfig(h.config, config); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root with spaces")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	addArgs := []string{"-c", h.config, "add", "--escape", "--label", `delegate:literal;$(echo no)`, "--print-task-id", "--", filepath.Join(t.TempDir(), "runner with spaces"), "--root", root, "0123456789abcdef0123456789abcdef"}
	code, stdout, stderr := captureRun(t, func() int { return RunWithExecutable(h.executable, h.config, addArgs) })
	if code != 0 || !bytes.Equal(stdout, []byte("42\n")) || len(stderr) != 0 {
		t.Fatalf("add result code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	entry, completion := readTestReceipts(t, h.artifact)
	if !equalStrings(entry.Argv, addArgs) || completion.StdoutSHA256 != hashOutput(stdout) {
		t.Fatalf("add argv/output receipt mismatch: %+v %+v", entry, completion)
	}
}

func TestStatusPreservesRawBytes(t *testing.T) {
	h := newTestHarness(t)
	rawStatus := []byte(`{"tasks":{"7":{"duplicate":"kept"},"7":{"duplicate":"raw"}},"groups":{}}`)
	if err := os.WriteFile(h.status, rawStatus, 0o600); err != nil {
		t.Fatal(err)
	}
	statusArgs := []string{"-c", h.config, "status", "--json"}
	code, stdout, stderr := captureRun(t, func() int { return RunWithExecutable(h.executable, h.config, statusArgs) })
	if code != 0 || !bytes.Equal(stdout, rawStatus) || len(stderr) != 0 {
		t.Fatalf("status did not preserve raw bytes: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func prepareStatusInput(t *testing.T, base, path, kind string) {
	t.Helper()
	switch kind {
	case "missing":
		return
	case "symlink":
		target := filepath.Join(base, "status-target.json")
		if writeErr := os.WriteFile(target, []byte(`{"tasks":{},"groups":{}}`), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if linkErr := os.Symlink(target, path); linkErr != nil {
			t.Fatal(linkErr)
		}
	case "oversized":
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if truncateErr := file.Truncate(int64(MaxOutputBytes) + 1); truncateErr != nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			t.Fatal(truncateErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	default:
		t.Fatalf("unknown default status input kind %q", kind)
	}
}

func TestStatusStdoutOverrideSkipsDefaultStatusInput(t *testing.T) {
	for _, defaultInput := range []string{"missing", "symlink", "oversized"} {
		t.Run(defaultInput, func(t *testing.T) {
			base, resolveErr := filepath.EvalSymlinks(t.TempDir())
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			configPath := filepath.Join(base, ConfigName)
			artifactDir := filepath.Join(base, "receipts")
			defaultStatusPath := filepath.Join(base, "status.json")
			explicitStdoutPath := filepath.Join(base, "explicit-status.json")
			expected := []byte("explicit status\n")
			if err := os.WriteFile(explicitStdoutPath, expected, 0o600); err != nil {
				t.Fatal(err)
			}
			prepareStatusInput(t, base, defaultStatusPath, defaultInput)

			cfg := DefaultConfig(configPath, artifactDir, defaultStatusPath)
			cfg.Status.StdoutPath = explicitStdoutPath
			cfg.Status.ExitCode = 17
			args := []string{"-c", configPath, "status", "--json"}
			stdout, stderr, code, err := executeCommand(cfg, args)
			if err != nil || code != 17 || !bytes.Equal(stdout, expected) || len(stderr) != 0 {
				t.Fatalf("status override result: stdout=%q stderr=%q code=%d err=%v", stdout, stderr, code, err)
			}
		})
	}
}

func TestKillAndRemoveUseConfiguredNumericIDs(t *testing.T) {
	h := newTestHarness(t)
	config, err := LoadConfig(h.config)
	if err != nil {
		t.Fatal(err)
	}
	config.Kill.ExitCode = 7
	if err := WriteConfig(h.config, config); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"kill", "remove"} {
		args := []string{"-c", h.config, verb, "0"}
		code, stdout, stderr := captureRun(t, func() int { return RunWithExecutable(h.executable, h.config, args) })
		wantCode := 0
		if verb == "kill" {
			wantCode = 7
		}
		if code != wantCode || len(stdout) != 0 || len(stderr) != 0 {
			t.Fatalf("%s result code=%d stdout=%q stderr=%q", verb, code, stdout, stderr)
		}
		entry, completion := readReceiptForArgs(t, h.artifact, args)
		if entry.Verb != verb || completion.ExitCode != wantCode {
			t.Fatalf("%s receipt mismatch: %+v %+v", verb, entry, completion)
		}
	}
}

func TestReleaseDelayAndOutputFileLimitsAreFinite(t *testing.T) {
	h := newTestHarness(t)
	release := filepath.Join(filepath.Dir(h.config), "release")
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdoutPath := filepath.Join(filepath.Dir(h.config), "add.stdout")
	if err := os.WriteFile(stdoutPath, []byte("raw add output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(h.config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Add = VerbConfig{Delay: 20 * time.Millisecond, ReleasePath: release, StdoutPath: stdoutPath, ExitCode: 3}
	if err := WriteConfig(h.config, cfg); err != nil {
		t.Fatal(err)
	}
	args := []string{"-c", h.config, "add", "--escape", "--label", "label", "--print-task-id", "--", "/tmp/runner", "--root", "/tmp/root", "0123456789abcdef0123456789abcdef"}
	started := time.Now()
	code, stdout, stderr := captureRun(t, func() int { return RunWithExecutable(h.executable, h.config, args) })
	if code != 3 || !bytes.Equal(stdout, []byte("raw add output\n")) || len(stderr) != 0 {
		t.Fatalf("configured add result code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed >= time.Second {
		t.Fatalf("unexpected finite rendezvous duration: %s", elapsed)
	}
}

func TestOutputFileLimitRefusesOversize(t *testing.T) {
	h := newTestHarness(t)
	oversized := filepath.Join(filepath.Dir(h.config), "oversized.out")
	file, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(int64(MaxOutputBytes) + 1)
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := readConfiguredOutput(oversized); !errors.Is(err, ErrOutputTooBig) {
		t.Fatalf("oversized output was accepted: %v", err)
	}
}

func TestMissingReleaseFileNaturallyTimesOut(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "release")
	started := time.Now()
	if err := waitForInput(missing, 0, 20*time.Millisecond); !errors.Is(err, ErrReleaseTimeout) {
		t.Fatalf("missing release result: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("release timeout was not finite: %s", elapsed)
	}
}

func TestConfigIsStrictAndBounded(t *testing.T) {
	h := newTestHarness(t)
	data, err := os.ReadFile(h.config)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	err = json.Unmarshal(data, &raw)
	if err != nil {
		t.Fatal(err)
	}
	raw["unknown"] = true
	unknown, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.config, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(h.config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown config field accepted: %v", err)
	}

	tooLarge := DefaultConfig(h.config, h.artifact, h.status)
	tooLarge.Version = strings.Repeat("v", MaxConfigBytes)
	if err := WriteConfig(h.config, tooLarge); !errors.Is(err, ErrConfigTooBig) {
		t.Fatalf("oversized config accepted: %v", err)
	}
}

func TestBuildStatusFixtureMatchesPueueParser(t *testing.T) {
	data, err := BuildStatusFixture([]StatusJob{{ID: 7, Label: "delegate:" + strings.Repeat("a", 32) + ":" + strings.Repeat("b", 32), State: "running"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := pueue.ParseStatus(data, "4.0.4")
	if err != nil || len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].State != pueue.StateRunning {
		t.Fatalf("status fixture did not satisfy parser: jobs=%+v err=%v", jobs, err)
	}
	if _, err := BuildStatusFixture([]StatusJob{{ID: -1, Label: "label", State: "running"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid fixture ID accepted: %v", err)
	}
}

func TestPueueBindUsesSeparateProductionAndFixtureConfig(t *testing.T) {
	h := newCompiledHarness(t)
	rootID := strings.Repeat("a", 32)
	taskID := strings.Repeat("b", 32)
	status, err := BuildStatusFixture([]StatusJob{{ID: 7, Label: "delegate:" + rootID + ":" + taskID, State: "running"}})
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(h.status, status, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	client := bindCompiledHarness(t, h)
	if client.Binding().ConfigPath != h.expected {
		t.Fatalf("binding used fixture config path: %+v", client.Binding())
	}
	identity := pueue.Identity{
		RootID: rootID, TaskID: taskID,
		SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
	}
	observation, err := client.Reconcile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.NumericTaskID != 7 || observation.State != pueue.StateRunning {
		t.Fatalf("unexpected separate-config observation: %+v", observation)
	}
	versionArgs := []string{"-c", h.expected, "--version"}
	entry, completion := readReceiptForArgs(t, h.artifact, versionArgs)
	if entry.FixtureConfigPath != h.config || entry.ConfigPath != h.expected || completion.FixtureConfigPath != h.config || completion.ConfigPath != h.expected {
		t.Fatalf("receipts did not preserve both config coordinates: %+v %+v", entry, completion)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
