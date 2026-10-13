package pueue

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatusCaptureStreamsLargeQueueThroughNaturalWait(t *testing.T) {
	data, want := statusStreamTestLargeQueue(t, 0)
	if len(data) <= MaxControlBytes {
		t.Fatalf("large status fixture has only %d bytes", len(data))
	}
	executable := finiteExecutable(t, "cat \"$1\"\nprintf 'status-complete\\n' >&2")
	cmd := exec.Command(executable, statusCaptureTestPayloadFile(t, data))
	var events []CommandEvent
	pending := startOwnedCaptured(cmd, "large-status", func(event CommandEvent) {
		events = append(events, event)
	}, FixtureVersion)
	result := statusCaptureTestWait(t, pending)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	statusCaptureTestExited(t, result, 0)
	statusCaptureTestSnapshot(t, result, want, "status-complete\n")
	statusCaptureTestEvents(t, events, "large-status", cmd.Args, result)
}

func TestStatusCaptureFinishesSingleObjectFixtureAfterNaturalExit(t *testing.T) {
	data := statusTestPayload(t, map[string]any{}, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 0},
	})
	executable := finiteExecutable(t, "cat \"$1\"")
	pending := startOwnedCaptured(
		exec.Command(executable, statusCaptureTestPayloadFile(t, data)),
		"single-status",
		nil,
		FixtureVersion,
	)
	result := statusCaptureTestWait(t, pending)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	statusCaptureTestExited(t, result, 0)
	statusCaptureTestSnapshot(t, result, QueueSnapshot{Groups: map[string]Group{
		"default": {Status: "Running", ParallelTasks: 0},
	}}, "")
}

func TestStatusCaptureDrainsParserFailureToNaturalExit(t *testing.T) {
	// The suffix exceeds a child pipe's capacity. A parser that closes or
	// abandons stdout prevents the fixture from writing its completion marker.
	padding := strings.Repeat("x", 3*MaxControlBytes)
	for _, tc := range []struct {
		name     string
		data     string
		wantErr  error
		exitCode int
	}{
		{name: "malformed status", data: "null\n" + padding, wantErr: ErrUnknown},
		{
			name:     "oversized record and nonzero exit",
			data:     `{"tasks":{},"groups":{},"future":"` + padding + `"}`,
			wantErr:  ErrControlLimit,
			exitCode: 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := finiteExecutable(t, "cat \"$1\"\nprintf 'drained' >\"$2\"\nprintf 'drained\\n' >&2\nexit \"$3\"")
			marker := filepath.Join(t.TempDir(), "completed")
			cmd := exec.Command(executable, statusCaptureTestPayloadFile(t, []byte(tc.data)), marker, strconv.Itoa(tc.exitCode))
			pending := startOwnedCaptured(cmd, "drain-status", nil, FixtureVersion)
			result := statusCaptureTestWait(t, pending)
			statusCaptureTestExited(t, result, tc.exitCode)
			if !errors.Is(result.Err, tc.wantErr) {
				t.Fatalf("capture error = %v, want %v", result.Err, tc.wantErr)
			}
			var exitErr *exec.ExitError
			if errors.As(result.Err, &exitErr) != (tc.exitCode != 0) {
				t.Fatalf("Wait error was not preserved: %v", result.Err)
			}
			statusCaptureTestSnapshot(t, result, QueueSnapshot{}, "drained\n")
			completed, err := os.ReadFile(marker)
			if err != nil || string(completed) != "drained" {
				t.Fatalf("fixture did not finish writing stdout: marker=%q err=%v", completed, err)
			}
		})
	}
}

func TestStatusCaptureStartFailureFinishesCapture(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-supervisor"))
	var events []CommandEvent
	pending := startOwnedCaptured(cmd, "failed-start", func(event CommandEvent) {
		events = append(events, event)
	}, FixtureVersion)
	result := statusCaptureTestWait(t, pending)
	if !errors.Is(result.Err, os.ErrNotExist) || !errors.Is(result.Err, ErrUnknown) {
		t.Fatalf("Start or empty-capture error was lost: %v", result.Err)
	}
	if result.Started || result.PID != 0 || result.ExitCode != -1 {
		t.Fatalf("failed Start reported process completion: %+v", result)
	}
	statusCaptureTestSnapshot(t, result, QueueSnapshot{}, "")
	statusCaptureTestEvents(t, events, "failed-start", cmd.Args, result)
}

func TestStatusCapturePendingResultSnapshotsAreIndependent(t *testing.T) {
	label := "immutable-label"
	data := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, label, statusStreamTestRunningState()),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 2}})
	want := QueueSnapshot{
		Jobs:   []Job{{ID: 7, Label: &label, State: StateRunning, Group: "default", originalCommand: "runner --root /tmp/task"}},
		Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 2}},
	}
	executable := finiteExecutable(t, "cat \"$1\"\nprintf 'immutable\\n' >&2")
	pending := startOwnedCaptured(exec.Command(executable, statusCaptureTestPayloadFile(t, data)), "immutable-status", nil, FixtureVersion)
	original := statusCaptureTestWait(t, pending)
	if original.Err != nil {
		t.Fatal(original.Err)
	}
	statusCaptureTestExited(t, original, 0)
	statusCaptureTestSnapshot(t, original, want, "immutable\n")

	// Retain independently obtained views before mutating them. This catches
	// sharing of the snapshot pointer, job slice, label pointer, or group map.
	views := make([]CommandResult, 8)
	completed := make([]bool, len(views))
	var readers sync.WaitGroup
	for index := range views {
		readers.Add(1)
		go func() {
			defer readers.Done()
			views[index], completed[index] = pending.Result()
		}()
	}
	readers.Wait()
	for index, view := range views {
		if !completed[index] {
			t.Fatalf("completed command became pending for reader %d", index)
		}
		statusCaptureTestSnapshot(t, view, want, "immutable\n")
		statusCaptureTestMutateResult(view)
	}
	statusCaptureTestSnapshot(t, original, want, "immutable\n")
	again := waitPending(t, pending)
	statusCaptureTestSnapshot(t, again, want, "immutable\n")
}

func TestAwaitReadyPendingClassifiesCompletedStatusCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "malformed status", err: ErrUnknown, wantErr: ErrBinding},
		{name: "record limit", err: ErrControlLimit, wantErr: errStatusControlLimit},
		{name: "wrapped record limit", err: errors.Join(ErrUnknown, ErrControlLimit), wantErr: errStatusControlLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := &Pending{
				done: make(chan struct{}),
				args: []string{"finite-supervisor", "status", "--json"},
				result: CommandResult{
					snapshot: &QueueSnapshot{},
					Stderr:   []byte("finished status probe"),
					Started:  true,
					PID:      1234,
					ExitCode: 0,
					Err:      tc.err,
				},
			}
			close(pending.done)
			got, err := awaitReadyPending(t.Context(), nil, pending)
			if !errors.Is(err, tc.wantErr) || !errors.Is(err, tc.err) {
				t.Fatalf("completed status error = %v, want classification %v and cause %v", err, tc.wantErr, tc.err)
			}
			if pendingFrom(err) != nil {
				t.Fatalf("completed status was reported in flight: %v", err)
			}
			if got.PID != pending.result.PID || got.Started != pending.result.Started || got.ExitCode != pending.result.ExitCode || string(got.Stderr) != string(pending.result.Stderr) {
				t.Fatalf("readiness lost settled process evidence: got=%+v want=%+v", got, pending.result)
			}
			if !reflect.DeepEqual(got.snapshot, pending.result.snapshot) {
				t.Fatalf("readiness lost its status snapshot: got=%+v want=%+v", got.snapshot, pending.result.snapshot)
			}
		})
	}
}

func statusCaptureTestPayloadFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func statusCaptureTestWait(t *testing.T, pending *Pending) CommandResult {
	t.Helper()
	// Allow race instrumentation and large fixtures a separate setup bound;
	// this timeout observes the owned process without canceling it.
	result, err := observeCommand(t.Context(), pending, 10*time.Second)
	if pendingFrom(err) != nil {
		t.Fatalf("finite status fixture did not complete: %v", err)
	}
	return result
}

func statusCaptureTestExited(t *testing.T, result CommandResult, exitCode int) {
	t.Helper()
	if !result.Started || result.PID <= 0 || result.ExitCode != exitCode {
		t.Fatalf("natural process result = %+v, want started process with exit %d", result, exitCode)
	}
}

func statusCaptureTestSnapshot(t *testing.T, result CommandResult, want QueueSnapshot, stderr string) {
	t.Helper()
	if result.snapshot == nil {
		t.Fatal("status capture omitted its parsed snapshot")
	}
	if len(result.Stdout) != 0 || string(result.Stderr) != stderr {
		t.Fatalf("capture retained raw status or lost stderr: stdout bytes=%d stderr=%q", len(result.Stdout), result.Stderr)
	}
	statusStreamTestSnapshot(t, *result.snapshot, want)
}

func statusCaptureTestMutateResult(result CommandResult) {
	*result.snapshot.Jobs[0].Label = "mutated label"
	result.snapshot.Jobs[0].ID = -1
	result.snapshot.Jobs[0].Group = "mutated group"
	result.snapshot.Jobs[0].originalCommand = "mutated command"
	result.snapshot.Groups["default"] = Group{Status: "Reset", ParallelTasks: 99}
	result.snapshot.Groups["added"] = Group{Status: "Paused"}
	result.snapshot.Jobs = append(result.snapshot.Jobs, Job{ID: 99})
	result.Stderr[0] = 'X'
}

func statusCaptureTestEvents(t *testing.T, got []CommandEvent, commandID string, args []string, result CommandResult) {
	t.Helper()
	want := []CommandEvent{{CommandID: commandID, Stage: "entry", Argv: args, ExitCode: -1}}
	if result.Started {
		want = append(want, CommandEvent{CommandID: commandID, Stage: "started", Argv: args, PID: result.PID, ExitCode: -1})
	}
	want = append(want, CommandEvent{CommandID: commandID, Stage: "completed", Argv: args, PID: result.PID, ExitCode: result.ExitCode})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capture lifecycle events = %+v, want %+v", got, want)
	}
}
