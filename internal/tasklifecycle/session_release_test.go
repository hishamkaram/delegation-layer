package tasklifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
	"golang.org/x/sys/unix"
)

func terminalContinuationSession(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord, *task.OutcomeRecord) {
	t.Helper()
	store, td, request := claimedContinuationSession(t, true)
	outcome, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if err := errors.Join(cleanupErr, collectErr); err != nil || outcome == nil {
		t.Fatalf("terminal fixture did not publish: %v", err)
	}
	return store, td, request, outcome
}

func claimedContinuationSession(t *testing.T, sealed bool) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
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
	td, request := testsupport.NewAppTaskInStore(t, store, strings.Repeat("b", 32), sealed, prior)
	if err = td.ClaimSession(request.Provider, prior.ConversationID); err != nil {
		t.Fatal(err)
	}
	return store, td, request
}

type releaseLockFault struct {
	taskdir.BaseFaultInjector
	runFailures   []error
	runCalls      int
	onRun         func()
	onMaintenance func()
}

func (f *releaseLockFault) OnLockFileBarrier(string) error { return nil }

func (f *releaseLockFault) OnLockParentDirBarrier(string) error { return nil }

func (f *releaseLockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".maintenance.lock" && operation != unix.LOCK_UN && f.onMaintenance != nil {
		f.onMaintenance()
	}
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

func TestContinuationSessionReleaseRetainsContentionWhenNextAttemptExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, td, request, outcome := terminalContinuationSession(t)
		deadline := time.Now().Add(100 * time.Millisecond)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		maintenanceCalls := 0
		fault := &releaseLockFault{runFailures: []error{unix.EWOULDBLOCK}, onMaintenance: func() {
			maintenanceCalls++
			if maintenanceCalls == 2 {
				time.Sleep(time.Until(deadline))
				synctest.Wait()
			}
		}}
		store.SetFaultInjector(fault)
		err := ReleaseContinuationSessionContext(ctx, td, request, outcome)
		store.SetFaultInjector(nil)
		if !errors.Is(err, task.ErrSessionBusy) || !errors.Is(err, task.ErrLockBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("next-attempt deadline replaced the prior ownership cause: %v", err)
		}
		if maintenanceCalls != 2 || fault.runCalls != 1 {
			t.Fatalf("expiry did not occur at the next attempt boundary: maintenance=%d proof=%d", maintenanceCalls, fault.runCalls)
		}
		assertSessionReleaseAbsent(t, store, request)
	})
}

func TestContinuationSessionReleaseDoesNotWaitForRealActiveRunner(t *testing.T) {
	for _, test := range []struct {
		name      string
		separate  bool
		published bool
	}{
		{name: "execution-owner"},
		{name: "separate-observer", separate: true},
		{name: "published-but-execution-owned", published: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, observer, request, outcome := activeContinuationSession(t, test.separate, test.published)
				started := time.Now()
				err := ReleaseContinuationSession(observer, request, outcome)
				if !errors.Is(err, task.ErrSessionBusy) || time.Since(started) != 0 {
					t.Fatalf("active execution lease received retry permission: elapsed=%s, err=%v", time.Since(started), err)
				}
				assertSessionReleaseAbsent(t, store, request)
			})
		})
	}
}

func activeContinuationSession(t *testing.T, separate, published bool) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord, *task.OutcomeRecord) {
	t.Helper()
	store, td, request := claimedContinuationSession(t, false)
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	outcome := &task.OutcomeRecord{EvidenceSHA256: task.ComputeSHA256([]byte("unpublished"))}
	if published {
		outcome = finalizeActiveContinuation(t, td)
	}
	if !separate {
		return store, td, request, outcome
	}
	observer, err := store.OpenTask(td.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := observer.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, observer, request, outcome
}

func finalizeActiveContinuation(t *testing.T, td *taskdir.TaskDir) *task.OutcomeRecord {
	t.Helper()
	if err := td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err := td.WriteRawFiles("answer", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	outcome, cleanupErr, collectErr := td.Finalize(task.FixturePredicateRef())
	if err := errors.Join(cleanupErr, collectErr); err != nil || outcome == nil {
		t.Fatalf("active terminal fixture did not publish: %v", err)
	}
	return outcome
}

func TestContinuationSessionReleaseDoesNotAttachPriorContentionToIOFault(t *testing.T) {
	store, td, request, outcome := terminalContinuationSession(t)
	fault := &releaseLockFault{runFailures: []error{unix.EWOULDBLOCK, unix.EIO}}
	store.SetFaultInjector(fault)
	err := ReleaseContinuationSessionContext(context.Background(), td, request, outcome)
	store.SetFaultInjector(nil)
	if !errors.Is(err, unix.EIO) || errors.Is(err, task.ErrLockBusy) || fault.runCalls != 2 {
		t.Fatalf("prior contention changed the later I/O fault: %v; attempts=%d", err, fault.runCalls)
	}
	assertSessionReleaseAbsent(t, store, request)
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
