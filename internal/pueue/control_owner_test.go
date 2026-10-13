package pueue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const controlOwnerCallerTestMode = "--test-control-owner-caller"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == controlOwnerCallerTestMode {
		os.Exit(runControlOwnerCallerTest(os.Args[2:]))
	}
	if code, handled := RunControlOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func runControlOwnerCallerTest(args []string) int {
	if len(args) != 4 {
		return 2
	}
	lockFile, err := os.OpenFile(args[3], os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 1
	}
	if err = unix.Flock(int(lockFile.Fd()), unix.LOCK_SH); err != nil {
		return 1
	}
	lock := &bootstrapLock{file: lockFile}
	ctx := withGenerationControlLease(context.Background(), lock)
	command := exec.Command(args[0], args[1], args[2])
	command.Dir = filepath.Dir(args[0])
	started := make(chan struct{}, 1)
	startControlOwnedCaptured(ctx, os.Args[0], command, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", func(event CommandEvent) {
		if event.Stage == "started" {
			started <- struct{}{}
		}
	}, "")
	select {
	case <-started:
		return 0
	case <-time.After(2 * time.Second):
		return 1
	}
}

func TestControlOwnerReturnsNaturalCommandResult(t *testing.T) {
	executable := finiteExecutable(t, "printf 'owned\\n'")
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	pending := startControlOwnedCaptured(
		t.Context(), os.Args[0], command,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, "",
	)
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err != nil {
		t.Fatalf("external owner result: %+v: %v", result, err)
	}
	if !result.Started || result.ExitCode != 0 || string(result.Stdout) != "owned\n" {
		t.Fatalf("unexpected external owner result: %+v", result)
	}
}

func TestControlOwnerReturnsStatusSnapshot(t *testing.T) {
	label := "delegate:root:task"
	data := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, label, statusStreamTestRunningState()),
	}, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 4},
	})
	payload := statusCaptureTestPayloadFile(t, data)
	executable := finiteExecutable(t, "cat \"$1\"")
	command := exec.Command(executable, payload)
	command.Dir = filepath.Dir(executable)
	pending := startControlOwnedCaptured(
		t.Context(), os.Args[0], command,
		"cccccccccccccccccccccccccccccccc", nil, FixtureVersion,
	)
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := QueueSnapshot{
		Jobs: []Job{{
			ID: 7, Label: &label, State: StateRunning, Group: "default",
			originalCommand: "runner --root /tmp/task",
		}},
		Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 4}},
	}
	if result.snapshot == nil || !reflect.DeepEqual(*result.snapshot, want) {
		t.Fatalf("status snapshot = %+v, want %+v", result.snapshot, want)
	}
}

func TestControlOwnerPreservesUnknownStatus(t *testing.T) {
	label := "delegate:root:locked"
	data := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, label, map[string]any{
			"Locked": map[string]any{
				"previous_status": map[string]any{
					"Queued": map[string]any{"enqueued_at": statusTestTime},
				},
			},
		}),
	}, map[string]any{})
	executable := finiteExecutable(t, "cat \"$1\"")
	command := exec.Command(executable, statusCaptureTestPayloadFile(t, data))
	command.Dir = filepath.Dir(executable)
	result, err := observeCommand(t.Context(), startControlOwnedCaptured(
		t.Context(), os.Args[0], command,
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", nil, FixtureVersion,
	), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.snapshot == nil || len(result.snapshot.Jobs) != 1 || result.snapshot.Jobs[0].State != StateUnknown {
		t.Fatalf("unknown state changed across owner protocol: %+v", result.snapshot)
	}
}

func TestControlOwnerPreservesCompletedRefusal(t *testing.T) {
	executable := finiteExecutable(t, "exit 7")
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	pending := startControlOwnedCaptured(
		t.Context(), os.Args[0], command,
		"dddddddddddddddddddddddddddddddd", nil, "",
	)
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err == nil || !completedExit(err) || result.ExitCode != 7 {
		t.Fatalf("completed refusal = %+v, %v", result, err)
	}
	acknowledged := commandAcknowledgment(result, err)
	if acknowledged == nil || *acknowledged {
		t.Fatalf("completed refusal acknowledgment = %v", acknowledged)
	}
}

func TestControlOwnerAndGenerationLeaseSurviveCallerExit(t *testing.T) {
	base := t.TempDir()
	marker := filepath.Join(base, "completed")
	release := filepath.Join(base, "release")
	lockPath := filepath.Join(base, "bootstrap.lock")
	executable := finiteExecutable(t, "while [ ! -e \"$2\" ]; do sleep 0.02; done\nprintf completed >\"$1\"")
	caller := exec.Command(os.Args[0], controlOwnerCallerTestMode, executable, marker, release, lockPath)
	if output, err := caller.CombinedOutput(); err != nil {
		t.Fatalf("caller process: %v: %s", err, output)
	}
	if available, err := exclusiveLockAvailable(lockPath); err != nil || available {
		t.Fatalf("generation lease after caller exit: available=%t err=%v", available, err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		markerData, markerErr := os.ReadFile(marker)
		available, lockErr := exclusiveLockAvailable(lockPath)
		if markerErr == nil && string(markerData) == "completed" && lockErr == nil && available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not complete naturally: marker=%q marker_err=%v lock_available=%t lock_err=%v", markerData, markerErr, available, lockErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exclusiveLockAvailable(path string) (available bool, resultErr error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
