package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"

	"golang.org/x/sys/unix"
)

const controlOwnerCallerTestMode = "--test-control-owner-caller"

const controlOwnerTestStartupTimeout = 10 * time.Second

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
	if len(args) < 4 {
		return 2
	}
	ctx := context.Background()
	for _, path := range args[3:] {
		lockFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return 1
		}
		if err = unix.Flock(int(lockFile.Fd()), unix.LOCK_SH); err != nil {
			return 1
		}
		ctx = withControlLockLease(ctx, &bootstrapLock{file: lockFile})
	}
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
	case <-time.After(controlOwnerTestStartupTimeout):
		return 1
	}
}

func TestControlOwnerReturnsNaturalCommandResult(t *testing.T) {
	executable := finiteExecutable(t, "printf 'owned\\n'")
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	pending := startControlOwnerTest(t, command, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	awaitControlOwnerTest(t, pending)
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
	pending := startControlOwnerTest(t, command, "cccccccccccccccccccccccccccccccc", FixtureVersion)
	awaitControlOwnerTest(t, pending)
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := QueueSnapshot{
		Jobs: []Job{protocol.WithOriginalCommand(Job{
			ID: 7, Label: &label, State: StateRunning, Group: "default",
		}, "runner --root /tmp/task")},
		Groups: map[string]Group{"default": {Status: "Running", ParallelTasks: 4}},
	}
	snapshot, ok := result.Snapshot()
	if !ok || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("status snapshot = %+v, want %+v", snapshot, want)
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
	pending := startControlOwnerTest(t, command, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", FixtureVersion)
	awaitControlOwnerTest(t, pending)
	result, err := observeCommand(t.Context(), pending, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := result.Snapshot()
	if !ok || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].State != StateUnknown {
		t.Fatalf("unknown state changed across owner protocol: %+v", snapshot)
	}
}

func TestControlOwnerPreservesCompletedRefusal(t *testing.T) {
	executable := finiteExecutable(t, "exit 7")
	command := exec.Command(executable)
	command.Dir = filepath.Dir(executable)
	pending := startControlOwnerTest(t, command, "dddddddddddddddddddddddddddddddd", "")
	awaitControlOwnerTest(t, pending)
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
	var releaseErr error
	releaseOnce := sync.OnceFunc(func() { releaseErr = os.WriteFile(release, []byte("release"), 0o600) })
	t.Cleanup(releaseOnce)
	lockPaths := []string{
		filepath.Join(base, "bootstrap.lock"),
		filepath.Join(base, "queue-writer-intent.lock"),
		filepath.Join(base, "queue-identity-gate.lock"),
		filepath.Join(base, "queue-identity.lock"),
	}
	executable := finiteExecutable(t, "while [ ! -e \"$2\" ]; do sleep 0.02; done\nprintf completed >\"$1\"")
	caller := exec.Command(os.Args[0], append([]string{controlOwnerCallerTestMode, executable, marker, release}, lockPaths...)...)
	if output, err := caller.CombinedOutput(); err != nil {
		t.Fatalf("caller process: %v: %s", err, output)
	}
	for _, lockPath := range lockPaths {
		if available, err := exclusiveLockAvailable(lockPath); err != nil || available {
			t.Fatalf("nested control lease %s after caller exit: available=%t err=%v", filepath.Base(lockPath), available, err)
		}
	}
	releaseOnce()
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	deadline := time.Now().Add(controlOwnerTestStartupTimeout)
	for {
		markerData, markerErr := os.ReadFile(marker)
		available, lockErr := allExclusiveLocksAvailable(lockPaths)
		if markerErr == nil && string(markerData) == "completed" && lockErr == nil && available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner did not complete naturally: marker=%q marker_err=%v lock_available=%t lock_err=%v", markerData, markerErr, available, lockErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestControlOwnerTestHelperJoinsOwnerBeforeEarlierCleanup checks that
// startControlOwnerTest joins the owner inside the calling test's lifetime:
// after the release, which follows the subtest's context cancellation, and
// before cleanups registered earlier, such as fixture directories. A helper
// without the join still passes whenever completion wins the race to the
// earlier cleanup, so this is not a deterministic mutation test. The parent
// cleanup owns the release and the final reap on every path, and runs before
// the fixture directories are removed.
func TestControlOwnerTestHelperJoinsOwnerBeforeEarlierCleanup(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	var releaseErr error
	releaseOnce := sync.OnceFunc(func() { releaseErr = os.WriteFile(release, []byte("release"), 0o600) })
	// The release marker alone orders completion. The bounded poll count keeps
	// the child finite when the release never appears; the interval is only
	// cadence and does not bound wall-clock time.
	const releasePollInterval = 20 * time.Millisecond
	releasePolls := strconv.Itoa(int(controlOwnerTestStartupTimeout / releasePollInterval))
	executable := finiteExecutable(t, "remaining=$2\nwhile [ ! -e \"$1\" ]; do\n\tif [ \"$remaining\" -le 0 ]; then exit 1; fi\n\tremaining=$((remaining - 1))\n\tsleep \"$3\"\ndone")
	var pending *Pending
	t.Cleanup(func() {
		releaseOnce()
		if releaseErr != nil {
			t.Errorf("release owner fixture: %v", releaseErr)
		}
		if pending == nil {
			return
		}
		if !awaitPendingNaturally(pending, controlOwnerTestStartupTimeout) {
			t.Errorf("external control owner remained in flight %s after release", controlOwnerTestStartupTimeout)
		}
		<-pending.Done()
	})
	t.Run("owner", func(t *testing.T) {
		t.Cleanup(func() {
			if pending == nil {
				return
			}
			select {
			case <-pending.Done():
			default:
				t.Error("owner completion was not joined before an earlier cleanup")
			}
		})
		context.AfterFunc(t.Context(), releaseOnce)
		command := exec.Command(executable, release, releasePolls, strconv.FormatFloat(releasePollInterval.Seconds(), 'f', -1, 64))
		command.Dir = filepath.Dir(executable)
		pending = startControlOwnerTest(t, command, "acacacacacacacacacacacacacacacac", "")
	})
	if pending == nil {
		return
	}
	if result, done := pending.Result(); !done || !result.Started || result.ExitCode != 0 || result.Err != nil {
		t.Errorf("released owner result = %+v, done = %t", result, done)
	}
}

func startControlOwnerTest(t *testing.T, command *exec.Cmd, commandID, statusVersion string) *Pending {
	t.Helper()
	started := make(chan struct{}, 1)
	pending := startControlOwnedCaptured(t.Context(), os.Args[0], command, commandID, func(event CommandEvent) {
		if event.Stage == "started" {
			started <- struct{}{}
		}
	}, statusVersion)
	// The owner child is finite, so join its sole completion before earlier
	// cleanups such as the fixture's TempDir run. The join cannot use t.Context,
	// which is canceled before cleanups; the bounded wait only reports a stuck
	// owner, and the package watchdog remains the last resort.
	t.Cleanup(func() {
		if !awaitPendingNaturally(pending, controlOwnerTestStartupTimeout) {
			t.Errorf("external control owner remained in flight %s after the test ended", controlOwnerTestStartupTimeout)
		}
		<-pending.Done()
	})
	timer := time.NewTimer(controlOwnerTestStartupTimeout)
	defer timer.Stop()
	select {
	case <-started:
		return pending
	case <-pending.Done():
		result, _ := pending.Result()
		if !result.Started {
			t.Fatalf("external control owner failed before command start: %+v", result)
		}
		return pending
	case <-timer.C:
		t.Fatal("external control owner did not start the command")
		return nil
	}
}

// awaitControlOwnerTest is setup for tests that assert an owner's terminal
// result. It waits for the canonical completion, so the caller's later
// observation retrieves an already completed result; that observation no
// longer measures completion latency. The guard only fails a stuck owner; it
// does not judge the command's error, and startControlOwnerTest's cleanup still
// joins the owner before teardown.
func awaitControlOwnerTest(t *testing.T, pending *Pending) {
	t.Helper()
	if !awaitPendingNaturally(pending, controlOwnerTestStartupTimeout) {
		t.Fatalf("external control owner did not complete within %s", controlOwnerTestStartupTimeout)
	}
}

func allExclusiveLocksAvailable(paths []string) (bool, error) {
	for _, path := range paths {
		available, err := exclusiveLockAvailable(path)
		if err != nil || !available {
			return false, err
		}
	}
	return true, nil
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
