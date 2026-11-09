package tasklifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
	"golang.org/x/sys/unix"
)

func terminalContinuationSession(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord, *task.OutcomeRecord) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	prior := &task.PriorSession{Provider: "fixture:test", ConversationID: "release-contention", PredecessorTaskID: strings.Repeat("a", 32)}
	td, request := testsupport.NewAppTaskInStore(t, store, strings.Repeat("b", 32), true, prior)
	if err = td.ClaimSession(request.Provider, prior.ConversationID); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if err = errors.Join(cleanupErr, collectErr); err != nil || outcome == nil {
		t.Fatalf("terminal fixture did not publish: %v", err)
	}
	return store, td, request, outcome
}

type releaseLockFault struct {
	taskdir.BaseFaultInjector
	runFailures []error
	runCalls    int
	onRun       func()
}

func (f *releaseLockFault) OnLockFileBarrier(string) error { return nil }

func (f *releaseLockFault) OnLockParentDirBarrier(string) error { return nil }

func (f *releaseLockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) != ".run.lock" || operation == unix.LOCK_UN {
		return nil
	}
	f.runCalls++
	if f.onRun != nil {
		f.onRun()
	}
	if len(f.runFailures) == 0 {
		return nil
	}
	err := f.runFailures[0]
	f.runFailures = f.runFailures[1:]
	return err
}

func TestContinuationSessionReleaseDoesNotRetryJoinedFaults(t *testing.T) {
	store, td, request, outcome := terminalContinuationSession(t)
	fault := &releaseLockFault{runFailures: []error{errors.Join(task.ErrLockBusy, unix.EIO)}}
	store.SetFaultInjector(fault)
	err := ReleaseContinuationSessionContext(context.Background(), td, request, outcome)
	store.SetFaultInjector(nil)
	if !errors.Is(err, unix.EIO) || !errors.Is(err, task.ErrLockBusy) || fault.runCalls != 1 {
		t.Fatalf("mixed contention/I/O fault was hidden or retried: %v; attempts=%d", err, fault.runCalls)
	}
	assertSessionReleaseAbsent(t, store, request)
	if err = ReleaseContinuationSession(td, request, outcome); err != nil {
		t.Fatalf("explicit release after the reported fault failed: %v", err)
	}
}

func TestContinuationReleaseContentionRequiresOnlyAcquisitionContention(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "lock-busy", err: task.ErrLockBusy, want: true},
		{name: "active-owner", err: task.ErrSessionBusy},
		{name: "independent-active-owner", err: errors.Join(task.ErrSessionBusy, task.ErrLockBusy)},
		{name: "missing-proof", err: errors.Join(task.ErrNoSeal, task.ErrLockBusy)},
		{name: "corrupt-proof", err: errors.Join(task.ErrEvidenceFault, task.ErrLockBusy)},
		{name: "unlock-io", err: errors.Join(task.ErrLockBusy, unix.EIO)},
		{name: "canceled", err: errors.Join(task.ErrLockBusy, context.Canceled)},
		{name: "deadline", err: errors.Join(task.ErrLockBusy, context.DeadlineExceeded)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := continuationReleaseContended(test.err); got != test.want {
				t.Fatalf("retry permission=%v, want %v for %v", got, test.want, test.err)
			}
		})
	}
}

func TestContinuationSessionReleaseHonorsCanceledCaller(t *testing.T) {
	store, td, request, outcome := terminalContinuationSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fault := &releaseLockFault{runFailures: []error{unix.EWOULDBLOCK}, onRun: cancel}
	store.SetFaultInjector(fault)
	err := ReleaseContinuationSessionContext(ctx, td, request, outcome)
	store.SetFaultInjector(nil)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, task.ErrLockBusy) || fault.runCalls != 1 {
		t.Fatalf("canceled release retried or lost its cause: %v; attempts=%d", err, fault.runCalls)
	}
	assertSessionReleaseAbsent(t, store, request)
}

func TestContinuationSessionReleaseHonorsExpiredCaller(t *testing.T) {
	store, td, request, outcome := terminalContinuationSession(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	fault := &releaseLockFault{}
	store.SetFaultInjector(fault)
	err := ReleaseContinuationSessionContext(ctx, td, request, outcome)
	store.SetFaultInjector(nil)
	if !errors.Is(err, context.DeadlineExceeded) || fault.runCalls != 0 {
		t.Fatalf("expired caller acquired a lease: %v; attempts=%d", err, fault.runCalls)
	}
	assertSessionReleaseAbsent(t, store, request)
}

func TestCollectionWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForCollectionContext(ctx, time.Now().Add(time.Minute)) {
		t.Fatal("canceled collection wait granted another attempt")
	}
}

func assertSessionReleaseAbsent(t *testing.T, store *taskdir.Store, request *task.TaskRecord) {
	t.Helper()
	directory := task.ComputeSHA256([]byte(request.Provider + ":" + request.PriorSession.ConversationID))
	path := filepath.Join(store.Root, "sessions", directory, request.TaskID+".release.json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsuccessful release changed durable session ownership: %v", err)
	}
}

func TestContinuationSessionReleaseRetriesOnlyTransientRunLockContention(t *testing.T) {
	for _, test := range []struct {
		name      string
		transient bool
	}{
		{name: "uncontended"},
		{name: "collector-contention", transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, td, request, outcome := terminalContinuationSession(t)
			fault := &releaseLockFault{}
			if test.transient {
				fault.runFailures = []error{unix.EWOULDBLOCK}
			}
			store.SetFaultInjector(fault)
			err := ReleaseContinuationSessionContext(context.Background(), td, request, outcome)
			store.SetFaultInjector(nil)
			if err != nil {
				t.Fatalf("terminal continuation release failed: %v", err)
			}
			if test.transient && fault.runCalls < 2 {
				t.Fatal("one-shot run-lock contention was not retried")
			}
			assertSessionReleaseIdempotent(t, store, td, request, outcome)
		})
	}
}

func assertSessionReleaseIdempotent(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir, request *task.TaskRecord, outcome *task.OutcomeRecord) {
	t.Helper()
	directory := task.ComputeSHA256([]byte(request.Provider + ":" + request.PriorSession.ConversationID))
	path := filepath.Join(store.Root, "sessions", directory, request.TaskID+".release.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ReleaseContinuationSession(td, request, outcome); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("idempotent release changed canonical evidence: %v", err)
	}
}
