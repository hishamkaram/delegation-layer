package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestLifetimeRecordsBindLaunchSetAndFenceTermination(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })

	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeCgroupV2,
		Assurance: task.LifetimeAssuranceExact,
		ScopePlan: "cgroup-plan-v1",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	registration, err := td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeCgroupV2,
		Assurance: task.LifetimeAssuranceExact,
		ScopeID:   "/sys/fs/cgroup/delegate-test",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	must(t, err)
	if registration.TaskID != td.TaskID || registration.Deadline != deadline.Format(time.RFC3339Nano) {
		t.Fatalf("unexpected registration: %+v", registration)
	}

	for sequence := uint64(1); sequence <= 2; sequence++ {
		_, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{
			Sequence: sequence, PID: 100 + int(sequence), ProcessStart: "native:start", PGID: 100 + int(sequence),
		})
		must(t, err)
	}
	launches, err := td.ReadLifetimeLaunches()
	must(t, err)
	if len(launches) != 2 || launches[0].Sequence != 1 || launches[1].Sequence != 2 {
		t.Fatalf("unexpected lifetime launches: %+v", launches)
	}

	termination, err := td.RecordLifetimeTermination(task.LifetimeTerminationBudget)
	must(t, err)
	if termination.LaunchCount != 2 || !termination.ScopeEmpty {
		t.Fatalf("unexpected termination: %+v", termination)
	}
	terminated, err := td.HasTerminatedLifetime(task.LifetimeTerminationBudget)
	must(t, err)
	if !terminated {
		t.Fatal("budget lifetime termination was not authoritative")
	}
	if _, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 3, PID: 103, ProcessStart: "native:start", PGID: 103}); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("launch after termination error = %v, want ErrTerminalTask", err)
	}
}

func TestLifetimePresentRecordsWithMissingPreparedInputAreNotAbsent(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-missing-prepared-input",
		BootID:    "missing-prepared-input-boot",
		Deadline:  deadline,
	})
	mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:missing-prepared-input",
		BootID:    "missing-prepared-input-boot",
		Deadline:  deadline,
	})
	must(t, os.Remove(filepath.Join(td.Dir, "brief.md")))

	ready, err := td.StopTerminationReady()
	if ready {
		t.Fatal("present lifetime records with missing prepared input were treated as queue-only absence")
	}
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing prepared input error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestLifetimeLaunchAndTerminationRetainIdentityOnMissingRegistration(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-missing-registration",
		BootID:    "missing-registration-boot",
		Deadline:  deadline,
	})
	mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:missing-registration",
		BootID:    "missing-registration-boot",
		Deadline:  deadline,
	})
	_, err := td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 1, PID: 1201, ProcessStart: "missing-registration-start", PGID: 1201})
	must(t, err)
	_, err = td.RecordLifetimeTermination(task.LifetimeTerminationCompleted)
	must(t, err)
	must(t, os.Remove(filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)))

	if _, err = td.ReadLifetimeLaunches(); !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch read error = %v, want identity mismatch and os.ErrNotExist", err)
	}
	if _, err = td.ReadLifetimeTermination(); !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("termination read error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestRecordLifetimeLaunchContextPreservesCancellation(t *testing.T) {
	t.Run("canceled during staging", func(t *testing.T) {
		store := testStore(t)
		td := preparedTask(t, store)
		owner, err := td.AcquireLifetimeOwner(t.Context())
		must(t, err)
		t.Cleanup(func() { must(t, owner.Release()) })
		deadline := time.Now().Add(time.Minute).UTC()
		recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopePlan: "process-group-plan-context-cancel",
			BootID:    "context-cancel-boot",
			Deadline:  deadline,
		})
		mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopeID:   "process-group:context-cancel",
			BootID:    "context-cancel-boot",
			Deadline:  deadline,
		})

		ctx, cancel := context.WithCancel(t.Context())
		store.SetFaultInjector(&cancelBeforeLifetimeStage{cancel: cancel})
		_, err = td.RecordLifetimeLaunchContext(ctx, LifetimeLaunchFacts{
			Sequence: 1, PID: 701, ProcessStart: "context-cancel-start", PGID: 701,
		})
		store.SetFaultInjector(nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled launch error = %v, want context.Canceled", err)
		}
		if _, statErr := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(1))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled launch record exists: %v", statErr)
		}
	})

	t.Run("expired before nested reads", func(t *testing.T) {
		store := testStore(t)
		td := preparedTask(t, store)
		owner, err := td.AcquireLifetimeOwner(t.Context())
		must(t, err)
		t.Cleanup(func() { must(t, owner.Release()) })
		deadline := time.Now().Add(time.Minute).UTC()
		recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopePlan: "process-group-plan-context-deadline",
			BootID:    "context-deadline-boot",
			Deadline:  deadline,
		})
		mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopeID:   "process-group:context-deadline",
			BootID:    "context-deadline-boot",
			Deadline:  deadline,
		})

		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		_, err = td.RecordLifetimeLaunchContext(ctx, LifetimeLaunchFacts{
			Sequence: 1, PID: 702, ProcessStart: "context-deadline-start", PGID: 702,
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired launch error = %v, want context.DeadlineExceeded", err)
		}
		if _, statErr := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(1))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expired launch record exists: %v", statErr)
		}
	})
}

type cancelBeforeLifetimeStage struct {
	BaseFaultInjector
	cancel context.CancelFunc
	stage  string
	once   sync.Once
}

func (f *cancelBeforeLifetimeStage) OnBeforeStageCreate(stage, _ string) error {
	f.stage = stage
	f.once.Do(f.cancel)
	return nil
}

func TestRecordLifetimeTerminationContextRejectsCancellationBeforeIO(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil", nil, context.Canceled},
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var td *TaskDir
			record, err := td.RecordLifetimeTerminationContext(tc.ctx, task.LifetimeTerminationRecovery)
			if record != nil || !errors.Is(err, tc.want) {
				t.Fatalf("termination = %+v, %v; want %v before I/O", record, err, tc.want)
			}
		})
	}
}

func TestRecordLifetimeTerminationContextCancellationBeforeStage(t *testing.T) {
	td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
	_, err := td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 1, PID: 702, ProcessStart: "termination-context-launch", PGID: 702})
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hook := &cancelBeforeControlStage{cancel: cancel}
	td.store.SetFaultInjector(hook)
	record, err := td.RecordLifetimeTerminationContext(ctx, task.LifetimeTerminationRecovery)
	td.store.SetFaultInjector(nil)
	if record != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("termination = %+v, %v; want cancellation before staging", record, err)
	}
	if hook.stage == "" {
		t.Fatal("cancellation did not reach the nested staging boundary")
	}
	for _, path := range []string{hook.stage, filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled termination left %s: %v", path, statErr)
		}
	}
	record, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationRecovery)
	must(t, err)
	if record.LaunchCount != 1 || !record.ScopeEmpty {
		t.Fatalf("retry lost the termination binding: %+v", record)
	}
}

func TestRecordLifetimeTerminationContextCompletesStartedWriteAndPreservesReplay(t *testing.T) {
	td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hook := &cancelAtLifetimeStageBarrier{cancel: cancel}
	td.store.SetFaultInjector(hook)
	record, err := td.RecordLifetimeTerminationContext(ctx, task.LifetimeTerminationRecovery)
	td.store.SetFaultInjector(nil)
	if record != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("termination = %+v, %v; want canceled final observation", record, err)
	}
	if hook.stage == "" {
		t.Fatal("termination did not reach durable staging")
	}
	if _, statErr := os.Lstat(hook.stage); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancellation interrupted owned stage cleanup: %v", statErr)
	}
	winner, err := td.ReadLifetimeTermination()
	must(t, err)
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)
	before := readTestFile(t, path)
	replayed, err := td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationRecovery)
	must(t, err)
	if !reflect.DeepEqual(replayed, winner) {
		t.Fatalf("termination replay changed winner: first=%+v replay=%+v", winner, replayed)
	}
	if _, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationBudget); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting termination replay error = %v, want identity mismatch", err)
	}
	if !reflect.DeepEqual(readTestFile(t, path), before) {
		t.Fatal("termination replay changed persisted winner bytes")
	}
}

type cancelAtLifetimeStageBarrier struct {
	BaseFaultInjector
	cancel context.CancelFunc
	stage  string
}

func (f *cancelAtLifetimeStageBarrier) OnBeforeStageCreate(stage, _ string) error {
	f.stage = stage
	return nil
}

func (f *cancelAtLifetimeStageBarrier) OnStageBarrier(_ string) error {
	f.cancel()
	return nil
}

type lifetimeContextWriter struct {
	name  string
	setup func(*testing.T) (*TaskDir, string)
	write func(context.Context, *TaskDir) error
	read  func(context.Context, *TaskDir) error
}

func lifetimeContextWriters() []lifetimeContextWriter {
	deadline := time.Date(2026, time.October, 11, 12, 0, 0, 0, time.UTC)
	intent := LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "context-writer-plan",
		BootID:    "context-writer-boot",
		Deadline:  deadline,
	}
	registration := LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "context-writer-scope",
		BootID:    intent.BootID,
		Deadline:  deadline,
	}
	worker := LifetimeWorkerFacts{PID: 913, ProcessStart: "context-writer-start", BootID: intent.BootID}
	wait := LifetimeWorkerWaitFacts{
		WorkerSHA256: "",
		ChildPID:     0,
		Result:       task.LifetimeWorkerWaitNotStarted,
		ObservedAt:   time.Date(2026, time.October, 11, 12, 1, 0, 0, time.UTC),
	}
	return []lifetimeContextWriter{
		{
			name: "intent",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeIntentName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeIntentContext(ctx, intent)
				return err
			},
			read: func(ctx context.Context, td *TaskDir) error {
				_, err := td.ReadLifetimeIntentContext(ctx)
				return err
			},
		},
		{
			name: "registration",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				recordTaskLifetimeIntent(t, td, intent)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeRegistrationContext(ctx, registration)
				return err
			},
			read: func(ctx context.Context, td *TaskDir) error {
				_, err := td.ReadLifetimeRegistrationContext(ctx)
				return err
			},
		},
		{
			name: "worker",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				recordTaskLifetimeIntent(t, td, intent)
				mustRecordLifetimeRegistration(t, td, registration)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeWorkerContext(ctx, worker)
				return err
			},
			read: func(ctx context.Context, td *TaskDir) error {
				_, err := td.ReadLifetimeWorkerContext(ctx)
				return err
			},
		},
		{
			name: "worker-wait",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				recordTaskLifetimeIntent(t, td, intent)
				mustRecordLifetimeRegistration(t, td, registration)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeWorkerWaitContext(ctx, wait)
				return err
			},
			read: func(ctx context.Context, td *TaskDir) error {
				_, err := td.ReadLifetimeWorkerWaitContext(ctx)
				return err
			},
		},
	}
}

func TestRecordLifetimeContextWritersRejectCancellationBeforeIO(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, writer := range lifetimeContextWriters() {
		t.Run(writer.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				ctx  context.Context
				want error
			}{
				{name: "nil", want: context.Canceled},
				{name: "canceled", ctx: canceled, want: context.Canceled},
				{name: "expired", ctx: expired, want: context.DeadlineExceeded},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var td *TaskDir
					err := writer.write(tc.ctx, td)
					if !errors.Is(err, tc.want) {
						t.Fatalf("writer error = %v, want %v before I/O", err, tc.want)
					}
				})
			}
		})
	}
}

func TestRecordLifetimeContextWritersCancelBeforeStagingWithoutWinner(t *testing.T) {
	for _, writer := range lifetimeContextWriters() {
		t.Run(writer.name, func(t *testing.T) {
			td, path := writer.setup(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hook := &cancelBeforeLifetimeStage{cancel: cancel}
			td.store.SetFaultInjector(hook)
			err := writer.write(ctx, td)
			td.store.SetFaultInjector(nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("writer error = %v, want context.Canceled", err)
			}
			if hook.stage == "" {
				t.Fatal("writer did not reach the staging boundary")
			}
			if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("canceled writer published %s: %v", path, statErr)
			}
		})
	}
}

func TestRecordLifetimeContextWritersCompleteStartedWriteAndReplayImmutable(t *testing.T) {
	for _, writer := range lifetimeContextWriters() {
		t.Run(writer.name, func(t *testing.T) {
			td, path := writer.setup(t)
			ctx, cancel := context.WithCancel(t.Context())
			hook := &cancelAtLifetimeStageBarrier{cancel: cancel}
			td.store.SetFaultInjector(hook)
			err := writer.write(ctx, td)
			td.store.SetFaultInjector(nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("writer error = %v, want context.Canceled after durable staging", err)
			}
			if hook.stage == "" {
				t.Fatal("writer did not reach durable staging")
			}
			if _, statErr := os.Lstat(hook.stage); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("writer left owned stage %s: %v", hook.stage, statErr)
			}
			if err := writer.read(t.Context(), td); err != nil {
				t.Fatalf("durable winner could not be replayed: %v", err)
			}
			before := readTestFile(t, path)
			if err := writer.write(t.Context(), td); err != nil {
				t.Fatalf("exact replay failed: %v", err)
			}
			if after := readTestFile(t, path); !reflect.DeepEqual(after, before) {
				t.Fatal("exact replay changed persisted winner bytes")
			}
		})
	}
}

func TestRecordLifetimeContextWritersPreservePersistenceDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "shorter caller deadline", timeout: controlOperationTimeout / 2},
		{name: "longer caller deadline", timeout: 2 * controlOperationTimeout},
		{name: "background fallback", timeout: 0},
	} {
		for _, writer := range lifetimeContextWriters() {
			writer := writer
			t.Run(tc.name+"/"+writer.name, func(t *testing.T) {
				testLifetimeContextWriterDeadline(t, writer, tc.timeout)
			})
		}
	}
}

func testLifetimeContextWriterDeadline(t *testing.T, writer lifetimeContextWriter, callerTimeout time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		td, path := writer.setup(t)
		must(t, td.store.maintLock.LockEX(t.Context()))
		ctx := context.Background()
		timeout := controlOperationTimeout
		if callerTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, callerTimeout)
			defer cancel()
			timeout = callerTimeout
		}
		completed := make(chan error, 1)
		go func() { completed <- writer.write(ctx, td) }()
		joined := false
		defer func() {
			unlockErr := td.store.maintLock.Unlock()
			if !joined {
				<-completed
			}
			must(t, unlockErr)
		}()
		synctest.Wait()
		time.Sleep(timeout / 2)
		synctest.Wait()
		select {
		case err := <-completed:
			joined = true
			t.Fatalf("writer stopped before selected deadline: %v", err)
		default:
		}
		time.Sleep(timeout - timeout/2)
		synctest.Wait()
		select {
		case err := <-completed:
			joined = true
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, task.ErrLockBusy) {
				t.Fatalf("writer error = %v, want selected maintenance-lock deadline", err)
			}
		default:
			t.Fatal("writer ignored its selected deadline")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired writer published a record: %v", err)
		}
	})
}

func TestRecordLifetimeTerminationContextPreservesPersistenceDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"shorter caller deadline", controlOperationTimeout / 2},
		{"longer caller deadline", 2 * controlOperationTimeout},
		{"background fallback", 0},
	} {
		t.Run(tc.name, func(t *testing.T) { testLifetimeTerminationDeadline(t, tc.timeout) })
	}
}

func testLifetimeTerminationDeadline(t *testing.T, callerTimeout time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
		must(t, td.store.maintLock.LockEX(t.Context()))
		ctx := context.Background()
		timeout := controlOperationTimeout
		if callerTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, callerTimeout)
			defer cancel()
			timeout = callerTimeout
		}
		completed := make(chan error, 1)
		go func() {
			_, err := td.RecordLifetimeTerminationContext(ctx, task.LifetimeTerminationRecovery)
			completed <- err
		}()
		joined := false
		defer func() {
			unlockErr := td.store.maintLock.Unlock()
			if !joined {
				<-completed
			}
			must(t, unlockErr)
		}()
		// Wait establishes lock contention. Sleep advances only synctest's
		// clock, so host scheduling cannot consume the operation's deadline.
		synctest.Wait()
		time.Sleep(timeout / 2)
		synctest.Wait()
		select {
		case err := <-completed:
			joined = true
			t.Fatalf("termination stopped before its selected deadline: %v", err)
		default:
		}
		time.Sleep(timeout - timeout/2)
		synctest.Wait()
		select {
		case err := <-completed:
			joined = true
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, task.ErrLockBusy) {
				t.Fatalf("termination error = %v, want the selected maintenance-lock deadline", err)
			}
		default:
			t.Fatal("termination ignored its selected deadline")
		}
		if _, err := os.Lstat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired termination published a record: %v", err)
		}
	})
}

type lifetimeContextRead struct {
	name string
	read func(context.Context) error
}

type lifetimeErrHookContext struct {
	context.Context
	onErr func(int)
	calls int
}

func (c *lifetimeErrHookContext) Err() error {
	c.calls++
	if c.onErr != nil {
		c.onErr(c.calls)
	}
	return c.Context.Err()
}

func readLifetimeAfterRemovingDependency(t *testing.T, path string, checksAfterReread int, read func(context.Context) error) error {
	t.Helper()
	calibration := &lifetimeErrHookContext{Context: t.Context()}
	if err := read(calibration); err != nil {
		t.Fatalf("calibration read: %v", err)
	}
	if calibration.calls <= checksAfterReread {
		t.Fatalf("calibration context Err calls = %d, want reread boundary", calibration.calls)
	}
	removeAt := calibration.calls - checksAfterReread
	var removeErr error
	actual := &lifetimeErrHookContext{
		Context: t.Context(),
		onErr: func(call int) {
			if call == removeAt {
				removeErr = os.Remove(path)
			}
		},
	}
	err := read(actual)
	if removeErr != nil {
		t.Fatalf("remove dependency at reread boundary: %v", removeErr)
	}
	return err
}

func lifetimeContextReads(td *TaskDir) []lifetimeContextRead {
	return []lifetimeContextRead{
		{"intent", func(ctx context.Context) error { _, err := td.ReadLifetimeIntentContext(ctx); return err }},
		{"registration", func(ctx context.Context) error { _, err := td.ReadLifetimeRegistrationContext(ctx); return err }},
		{"registration-digest", func(ctx context.Context) error { _, err := td.LifetimeRegistrationDigestContext(ctx); return err }},
		{"worker", func(ctx context.Context) error { _, err := td.ReadLifetimeWorkerContext(ctx); return err }},
		{"worker-digest", func(ctx context.Context) error { _, err := td.LifetimeWorkerDigestContext(ctx); return err }},
		{"worker-wait", func(ctx context.Context) error { _, err := td.ReadLifetimeWorkerWaitContext(ctx); return err }},
		{"worker-wait-binding", func(ctx context.Context) error {
			_, _, _, _, err := td.lifetimeWorkerWaitBindingContext(ctx)
			return err
		}},
		{"launch", func(ctx context.Context) error { _, err := td.readLifetimeLaunchContext(ctx, 1); return err }},
		{"launches", func(ctx context.Context) error { _, err := td.ReadLifetimeLaunchesContext(ctx); return err }},
		{"termination", func(ctx context.Context) error { _, err := td.ReadLifetimeTerminationContext(ctx); return err }},
		{"has-terminated", func(ctx context.Context) error { _, err := td.HasTerminatedLifetimeContext(ctx, ""); return err }},
		{"stop-ready", func(ctx context.Context) error { _, err := td.StopTerminationReadyContext(ctx); return err }},
	}
}

func TestLifetimeContextReadsRejectCancellationBeforeIO(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil", nil, context.Canceled},
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No filesystem handle is available: reaching I/O would panic instead
			// of returning the caller's cancellation or deadline error.
			for _, operation := range lifetimeContextReads(nil) {
				t.Run(operation.name, func(t *testing.T) {
					if err := operation.read(tc.ctx); !errors.Is(err, tc.want) {
						t.Fatalf("read error = %v, want %v before I/O", err, tc.want)
					}
				})
			}
		})
	}
}

func TestLifetimeDigestRereadAbsenceIsValidationDependency(t *testing.T) {
	t.Run("registration-digest", func(t *testing.T) {
		td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
		path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
		err := readLifetimeAfterRemovingDependency(t, path, 2, func(ctx context.Context) error {
			_, err := td.LifetimeRegistrationDigestContext(ctx)
			return err
		})
		if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("registration digest error = %v, want identity mismatch and os.ErrNotExist", err)
		}
	})

	t.Run("worker-digest", func(t *testing.T) {
		td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
		err := readLifetimeAfterRemovingDependency(t, path, 1, func(ctx context.Context) error {
			_, err := td.LifetimeWorkerDigestContext(ctx)
			return err
		})
		if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worker digest error = %v, want identity mismatch and os.ErrNotExist", err)
		}
	})

	t.Run("worker-registration-binding", func(t *testing.T) {
		td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
		err := readLifetimeAfterRemovingDependency(t, path, 2, func(ctx context.Context) error {
			_, err := td.ReadLifetimeWorkerContext(ctx)
			return err
		})
		if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worker registration binding error = %v, want identity mismatch and os.ErrNotExist", err)
		}
	})
}

func TestLifetimeContextReadsPreserveDurableBindings(t *testing.T) {
	td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
	_, err := td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 1, PID: 701, ProcessStart: "context-read-launch", PGID: 701})
	must(t, err)
	wait, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest, ChildPID: worker.PID,
		Result: task.LifetimeWorkerWaitSuccess, ExitCode: taskdirIntPointer(0),
	})
	must(t, err)
	_, err = td.RecordLifetimeTermination(task.LifetimeTerminationRecovery)
	must(t, err)
	for _, operation := range lifetimeContextReads(td) {
		t.Run(operation.name, func(t *testing.T) { must(t, operation.read(t.Context())) })
	}
	readWorker, err := td.ReadLifetimeWorkerContext(t.Context())
	must(t, err)
	readWait, err := td.ReadLifetimeWorkerWaitContext(t.Context())
	must(t, err)
	digest, err := td.LifetimeWorkerDigestContext(t.Context())
	must(t, err)
	if !reflect.DeepEqual(worker, readWorker) || !reflect.DeepEqual(wait, readWait) || digest != workerDigest {
		t.Fatalf("context reads changed the durable worker/Wait binding: worker=%+v wait=%+v digest=%q", readWorker, readWait, digest)
	}
	terminated, err := td.HasTerminatedLifetimeContext(t.Context(), task.LifetimeTerminationRecovery)
	must(t, err)
	ready, err := td.StopTerminationReadyContext(t.Context())
	must(t, err)
	if !terminated || !ready {
		t.Fatalf("context reads lost completed lifetime authority: terminated=%t ready=%t", terminated, ready)
	}
	terminated, err = td.HasTerminatedLifetimeContext(t.Context(), task.LifetimeTerminationBudget)
	must(t, err)
	if terminated {
		t.Fatal("context read accepted the wrong termination cause")
	}
}

func TestLifetimeMissingReadCannotEstablishAbsenceAfterCancellation(t *testing.T) {
	td := preparedTask(t, testStore(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := td.ReadLifetimeTerminationContext(ctx)
	if !errors.Is(err, os.ErrNotExist) || !task.RecordAbsent(err) {
		t.Fatalf("uncanceled missing-record read lost absence: %v", err)
	}
	cancel()
	// The store retains both errors when cancellation accompanies a missing
	// local read. A lifecycle decision must not discard that cancellation.
	interrupted := errors.Join(err, ctx.Err())
	if task.RecordAbsent(interrupted) {
		t.Fatal("canceled read established lifecycle absence")
	}
}

func TestStopObservationContextRejectsCancellationBeforeLifetimeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var td *TaskDir
	if err := td.recordStopObservationOwned(ctx, "budget", task.StopObservationFacts{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop observation error = %v, want cancellation before lifetime I/O", err)
	}
}

func TestRecordLifetimeLaunchContextRetainsMaintenanceDeadline(t *testing.T) {
	td, _, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
	store := td.store
	must(t, store.maintLock.LockEX(t.Context()))
	locked := true
	defer func() {
		if locked {
			must(t, store.maintLock.Unlock())
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	facts := LifetimeLaunchFacts{Sequence: 1, PID: 702, ProcessStart: "context-lock-deadline", PGID: 702}
	completed := make(chan error, 1)
	go func() {
		_, err := td.RecordLifetimeLaunchContext(ctx, facts)
		completed <- err
	}()
	var got error
	select {
	case got = <-completed:
		must(t, store.maintLock.Unlock())
		locked = false
	case <-time.After(2 * time.Second):
		must(t, store.maintLock.Unlock())
		locked = false
		<-completed
		t.Fatal("lifetime launch ignored the caller deadline while maintenance was held")
	}
	if !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, task.ErrLockBusy) {
		t.Fatalf("contended lifetime launch error = %v, want bounded lock deadline", got)
	}
	if _, err := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired launch published a record: %v", err)
	}
	recorded, err := td.RecordLifetimeLaunchContext(t.Context(), facts)
	must(t, err)
	if recorded.Sequence != facts.Sequence || recorded.PID != facts.PID {
		t.Fatalf("retry changed the launch identity: %+v", recorded)
	}
}

func TestTaskLifetimeRegistrationPostLinkErrorRequiresExactReplay(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	deadline := time.Now().Add(time.Minute).UTC()
	facts := LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:post-link",
		BootID:    "boot-token",
		Deadline:  deadline,
	}
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "process-group-plan-post-link",
		BootID:    facts.BootID,
		Deadline:  deadline,
	})

	store.SetFaultInjector(&orderedFault{target: lifetimeRegistrationName, fail: "post-link"})
	if _, err = td.RecordLifetimeRegistration(facts); err == nil {
		t.Fatal("post-link registration fault was not reported")
	}
	store.SetFaultInjector(nil)
	winner, err := td.ReadLifetimeRegistration()
	must(t, err)
	replayed, err := td.RecordLifetimeRegistration(facts)
	must(t, err)
	if replayed.ScopeID != winner.ScopeID || replayed.CreatedAt != winner.CreatedAt {
		t.Fatalf("registration replay changed winner: first=%+v replay=%+v", winner, replayed)
	}
}

func TestTaskLifetimeWorkerPostLinkErrorRequiresExactReplay(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group:worker-post-link",
		BootID:    "worker-post-link-boot",
		Deadline:  deadline,
	})
	mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:worker-post-link",
		BootID:    "worker-post-link-boot",
		Deadline:  deadline,
	})
	facts := LifetimeWorkerFacts{PID: 404, ProcessStart: "worker-post-link-start", BootID: "worker-post-link-boot"}
	fault := &orderedFault{target: lifetimeWorkerName, fail: "post-link"}
	store.SetFaultInjector(fault)
	if _, err = td.RecordLifetimeWorker(facts); !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("worker post-link error = %v, want uncertain durability", err)
	}
	winner, err := td.ReadLifetimeWorker()
	must(t, err)
	store.SetFaultInjector(nil)
	replayed, err := td.RecordLifetimeWorker(facts)
	must(t, err)
	if !reflect.DeepEqual(replayed, winner) {
		t.Fatalf("worker replay changed winner: first=%+v replay=%+v", winner, replayed)
	}
}

func TestLifetimeLaunchesRequireContiguousSequence(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-v1",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	_, err = td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:test",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	must(t, err)
	_, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{Sequence: 2, PID: 102, ProcessStart: "native:start", PGID: 102})
	must(t, err)
	if _, err = td.ReadLifetimeLaunches(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("sequence gap error = %v, want identity mismatch", err)
	}
}

func TestLifetimeLaunchWriterRejectsSharedLimitBeforeCreatingRecord(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-limit",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	_, err = td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:limit",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	must(t, err)

	sequence := task.MaxLifetimeLaunches + 1
	_, err = td.RecordLifetimeLaunch(LifetimeLaunchFacts{
		Sequence: sequence, PID: 1065, ProcessStart: "native:start", PGID: 1065,
	})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("overflow write error = %v, want evidence fault", err)
	}
	if _, statErr := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(sequence))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("overflow launch record exists: %v", statErr)
	}
}

func TestStopTerminationReadyBlocksIntentWithoutRegistration(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-pending",
		BootID:    "boot-token",
		Deadline:  deadline,
	})

	ready, err := td.StopTerminationReady()
	must(t, err)
	if ready {
		t.Fatal("intent-only lifetime was treated as historical termination")
	}
}

func TestLifetimeOwnerReleaseIsIdempotent(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	must(t, owner.Release())
	must(t, owner.Release())
}

func TestLifetimeOwnerDuplicatesHeldControlLeases(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	duplicates, err := owner.DuplicateControlLeaseFiles()
	must(t, err)
	if len(duplicates) != 2 {
		t.Fatalf("control lease descriptor count = %d, want 2", len(duplicates))
	}
	for _, duplicate := range duplicates {
		must(t, duplicate.Close())
	}
	if _, err = td.TryAcquireLifetimeOwner(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("duplicating leases released the lifetime owner: %v", err)
	}
	must(t, owner.Release())
	if _, err = owner.DuplicateControlLeaseFiles(); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatalf("released owner supplied control leases: %v", err)
	}
	reacquired, err := td.TryAcquireLifetimeOwner()
	must(t, err)
	must(t, reacquired.Release())
}

func TestLifetimeOwnerReleaseRetriesOwnerClose(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })

	closeErr := errors.New("injected lifetime owner close failure")
	fault := &lifetimeOwnerReleaseFault{
		failPath: lifetimeOwnerLockName,
		failures: []error{closeErr},
	}
	store.SetFaultInjector(fault)
	releaseErr := owner.Release()
	if !errors.Is(releaseErr, closeErr) {
		t.Fatalf("first release error = %v, want injected owner close failure (calls=%v)", releaseErr, fault.calls)
	}
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 1 {
		t.Fatalf("owner unlock calls after failed release = %d, want 1", got)
	}
	if got := fault.unlockCalls(".maintenance.lock"); got != 1 {
		t.Fatalf("maintenance unlock calls after owner close failure = %d, want 1", got)
	}
	probe, err := store.openExistingLock(filepath.Join(td.Dir, lifetimeDirectory, lifetimeOwnerLockName), LockLevelRun)
	must(t, err)
	if err = probe.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("owner remained acquirable after close failure: %v", err)
	}
	must(t, probe.Close())

	must(t, owner.Release())
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 2 {
		t.Fatalf("owner unlock calls after retry = %d, want 2", got)
	}
	if got := fault.unlockCalls(".maintenance.lock"); got != 1 {
		t.Fatalf("maintenance unlock calls after owner close retry = %d, want 1", got)
	}
	must(t, owner.Release())
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 2 {
		t.Fatalf("owner unlock calls after idempotent release = %d, want 2", got)
	}

	reacquired, err := td.TryAcquireLifetimeOwner()
	must(t, err)
	must(t, reacquired.Release())
}

func TestLifetimeOwnerReleaseRetriesMaintenanceUnlock(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })

	unlockErr := errors.New("injected maintenance unlock failure")
	fault := &lifetimeOwnerReleaseFault{
		failPath: ".maintenance.lock",
		failures: []error{unlockErr},
	}
	store.SetFaultInjector(fault)
	if err = owner.Release(); !errors.Is(err, unlockErr) {
		t.Fatalf("first release error = %v, want injected maintenance unlock failure", err)
	}
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 1 {
		t.Fatalf("owner unlock calls after failed maintenance unlock = %d, want 1", got)
	}
	if got := fault.unlockCalls(".maintenance.lock"); got != 1 {
		t.Fatalf("maintenance unlock calls after failed release = %d, want 1", got)
	}
	other, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	if err = other.maintLock.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("maintenance lock was released after injected failure: %v", err)
	}

	must(t, owner.Release())
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 1 {
		t.Fatalf("owner unlock calls after maintenance retry = %d, want 1", got)
	}
	if got := fault.unlockCalls(".maintenance.lock"); got != 2 {
		t.Fatalf("maintenance unlock calls after retry = %d, want 2", got)
	}
	must(t, owner.Release())
	if got := fault.unlockCalls(lifetimeOwnerLockName); got != 1 {
		t.Fatalf("owner unlock calls after idempotent release = %d, want 1", got)
	}
	if got := fault.unlockCalls(".maintenance.lock"); got != 2 {
		t.Fatalf("maintenance unlock calls after idempotent release = %d, want 2", got)
	}

	must(t, other.maintLock.LockEXNonblocking())
	must(t, other.maintLock.Unlock())
}

type lifetimeOwnerReleaseFault struct {
	BaseFaultInjector
	failPath string
	failures []error
	calls    map[string]int
}

func (f *lifetimeOwnerReleaseFault) OnLockFileBarrier(string) error {
	return nil
}

func (f *lifetimeOwnerReleaseFault) OnLockParentDirBarrier(string) error {
	return nil
}

func (f *lifetimeOwnerReleaseFault) OnFlock(path string, operation int) error {
	if operation != unix.LOCK_UN {
		return nil
	}
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	name := filepath.Base(path)
	f.calls[name]++
	if name != f.failPath || len(f.failures) == 0 {
		return nil
	}
	err := f.failures[0]
	f.failures = f.failures[1:]
	return err
}

func (f *lifetimeOwnerReleaseFault) unlockCalls(name string) int {
	return f.calls[name]
}

func TestTryAcquireLifetimeOwnerContextRejectsCancellationBeforeMutation(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := td.TryAcquireLifetimeOwnerContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition error = %v, want context canceled", err)
	}
	if _, err := os.Stat(filepath.Join(td.Dir, lifetimeDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled acquisition changed lifetime state: %v", err)
	}
}

func TestAcquireLifetimeOwnerRejectsCancellationBeforeMutation(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := td.AcquireLifetimeOwner(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition error = %v, want context canceled", err)
	}
	if _, err := os.Stat(filepath.Join(td.Dir, lifetimeDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled acquisition changed lifetime state: %v", err)
	}
}

func TestRegisteredLifetimeBlocksQueueEndUntilScopeIsEmpty(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-v1",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	_, err = td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:test",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	must(t, err)
	worker, err := td.RecordLifetimeWorker(LifetimeWorkerFacts{
		PID: 701, ProcessStart: "worker-start", BootID: "boot-token",
	})
	must(t, err)
	workerDigest, err := td.LifetimeWorkerDigest()
	must(t, err)
	start := consumeStart(t, td)
	must(t, start.Release())
	requestID := "budget"
	permit, err := td.PrepareStop(requestID, "budget", time.Now().Add(time.Minute))
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	must(t, td.RecordStopReply(requestID, task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true}))
	facts := task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}
	if err = td.RecordStopObservation(requestID, facts); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("queue end without lifetime proof error = %v, want ErrSessionBusy", err)
	}
	_, err = td.RecordLifetimeTermination(task.LifetimeTerminationBudget)
	must(t, err)
	must(t, owner.Release())
	if err = td.RecordStopObservation(requestID, facts); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("queue end without exact worker Wait error = %v, want ErrSessionBusy", err)
	}
	waitOwner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	_, err = td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     taskdirIntPointer(0),
	})
	must(t, err)
	must(t, waitOwner.Release())
	must(t, td.RecordStopObservation(requestID, facts))
}

func recordTaskLifetimeIntent(t *testing.T, td *TaskDir, facts LifetimeIntentFacts) {
	t.Helper()
	_, err := td.RecordLifetimeIntent(facts)
	must(t, err)
}

func TestLifetimeRegistrationRequiresIntentForNewWrites(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	deadline := time.Now().Add(time.Minute).UTC()
	_, err := td.RecordLifetimeRegistration(LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeCgroupV2,
		Assurance: task.LifetimeAssuranceExact,
		ScopeID:   "/sys/fs/cgroup/delegate-test",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration without intent error = %v, want missing intent", err)
	}
}

func TestLifetimeLegacyRegistrationReadRemainsCompatible(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	_, _, _, spec, meta, err := td.loadAndValidatePreparedSet()
	must(t, err)
	owner, err := task.LifetimeTaskOwnerSHA256(spec, meta)
	must(t, err)
	legacy := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        td.TaskID,
		OwnerKind:     task.LifetimeOwnerTask,
		OwnerSHA256:   owner,
		ScopeKind:     task.LifetimeScopeProcessGroup,
		Assurance:     task.LifetimeAssuranceCooperative,
		ScopeID:       "process-group:legacy",
		BootID:        "boot-token",
		Deadline:      time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	must(t, task.ValidateLifetimeRegistrationRecord(&legacy))
	must(t, td.store.mkdir(filepath.Join(td.Dir, lifetimeDirectory), td.store.faultInjector))
	must(t, td.recordSame(filepath.Join(lifetimeDirectory, lifetimeRegistrationName), legacy, func() (bool, error) {
		return false, os.ErrNotExist
	}))
	read, err := td.ReadLifetimeRegistration()
	must(t, err)
	if *read != legacy {
		t.Fatalf("legacy registration changed on read: got=%+v want=%+v", read, legacy)
	}
}

func TestLifetimeWorkerRequiresRegistrationAndIsCreateOnce(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	if _, err := td.RecordLifetimeWorker(LifetimeWorkerFacts{PID: 123, ProcessStart: "native:1", BootID: "boot-token"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker without registration error = %v, want missing registration", err)
	}
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "process-group-plan-v1",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "process-group:worker",
		BootID:    "boot-token",
		Deadline:  deadline,
	})
	first, err := td.RecordLifetimeWorker(LifetimeWorkerFacts{PID: 123, ProcessStart: "native:1", BootID: "boot-token"})
	must(t, err)
	replay, err := td.RecordLifetimeWorker(LifetimeWorkerFacts{PID: 123, ProcessStart: "native:1", BootID: "boot-token"})
	must(t, err)
	if *replay != *first {
		t.Fatalf("worker replay changed winner: first=%+v replay=%+v", first, replay)
	}
	if _, err = td.RecordLifetimeWorker(LifetimeWorkerFacts{PID: 124, ProcessStart: "native:2", BootID: "boot-token"}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting worker error = %v, want identity mismatch", err)
	}
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
	data, err := os.ReadFile(path)
	must(t, err)
	var tampered task.LifetimeWorkerRecord
	must(t, task.DecodeStrict(data, &tampered))
	tampered.RegistrationSHA256 = task.ComputeSHA256([]byte("other-registration"))
	data, err = task.MarshalCanonical(tampered)
	must(t, err)
	must(t, os.WriteFile(path, data, 0o600))
	if _, err = td.ReadLifetimeWorker(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("tampered worker digest error = %v, want identity mismatch", err)
	}
}

func TestLifetimeWorkerWaitRecordsValidResultsAndQualification(t *testing.T) {
	observedAt := time.Date(2026, time.October, 11, 11, 30, 0, 0, time.UTC)
	t.Run("exact-unqualified", func(t *testing.T) { assertUnqualifiedLifetimeWorkerWait(t, observedAt) })
	t.Run("cooperative-not-started", func(t *testing.T) {
		td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
		if worker != nil || workerDigest != "" {
			t.Fatal("not-started fixture unexpectedly persisted a worker")
		}
		first, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
			Result: task.LifetimeWorkerWaitNotStarted, ObservedAt: observedAt,
		})
		must(t, err)
		if !task.LifetimeWorkerWaitProvesNotStarted(first) {
			t.Fatalf("worker disposition = %+v, want not-started", first)
		}
		read, err := td.ReadLifetimeWorkerWait()
		must(t, err)
		if !reflect.DeepEqual(first, read) {
			t.Fatalf("not-started read changed winner: first=%+v read=%+v", first, read)
		}
	})

	for _, test := range []struct {
		name     string
		result   task.LifetimeWorkerWaitResult
		exitCode *int
	}{
		{name: "success", result: task.LifetimeWorkerWaitSuccess, exitCode: taskdirIntPointer(0)},
		{name: "exit-error", result: task.LifetimeWorkerWaitExitError, exitCode: taskdirIntPointer(-1)},
		{name: "wait-error", result: task.LifetimeWorkerWaitError},
	} {
		t.Run("qualified-"+test.name, func(t *testing.T) {
			assertQualifiedLifetimeWorkerWait(t, observedAt, test.result, test.exitCode)
		})
	}
}

func TestStopTerminationReadyAcceptsDurableNotStartedDisposition(t *testing.T) {
	td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
	if worker != nil || workerDigest != "" {
		t.Fatal("not-started fixture unexpectedly persisted a worker")
	}
	_, err := td.RecordLifetimeTermination(task.LifetimeTerminationRecovery)
	must(t, err)
	ready, err := td.StopTerminationReady()
	must(t, err)
	if ready {
		t.Fatal("termination became ready before worker completion disposition")
	}
	_, err = td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{Result: task.LifetimeWorkerWaitNotStarted})
	must(t, err)
	ready, err = td.StopTerminationReady()
	must(t, err)
	if !ready {
		t.Fatal("termination stayed pending after durable not-started disposition")
	}
}

func assertUnqualifiedLifetimeWorkerWait(t *testing.T, observedAt time.Time) {
	t.Helper()
	td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeCgroupV2, task.LifetimeAssuranceExact, false)
	if worker != nil || workerDigest != "" {
		t.Fatal("exact fixture unexpectedly persisted a worker")
	}
	first, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		ChildPID: 701, Result: task.LifetimeWorkerWaitSuccess,
		ExitCode: taskdirIntPointer(0), ObservedAt: observedAt,
	})
	must(t, err)
	if task.LifetimeWorkerWaitIsQualified(first) || first.WorkerSHA256 != "" {
		t.Fatalf("exact wait unexpectedly qualified: %+v", first)
	}
	if first.ObservedAt != observedAt.Format(time.RFC3339Nano) {
		t.Fatalf("observed timestamp = %q, want %q", first.ObservedAt, observedAt.Format(time.RFC3339Nano))
	}
	read, err := td.ReadLifetimeWorkerWait()
	must(t, err)
	if !reflect.DeepEqual(first, read) {
		t.Fatalf("unqualified wait read changed winner: first=%+v read=%+v", first, read)
	}
}

func assertQualifiedLifetimeWorkerWait(t *testing.T, observedAt time.Time, result task.LifetimeWorkerWaitResult, exitCode *int) {
	t.Helper()
	td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
	if worker == nil || workerDigest == "" {
		t.Fatal("qualified fixture did not persist a worker")
	}
	first, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest, ChildPID: worker.PID, Result: result,
		ExitCode: exitCode, ObservedAt: observedAt,
	})
	must(t, err)
	if !task.LifetimeWorkerWaitIsQualified(first) || first.WorkerSHA256 != workerDigest {
		t.Fatalf("qualified wait binding = %+v, want worker %q", first, workerDigest)
	}
	read, err := td.ReadLifetimeWorkerWait()
	must(t, err)
	if !reflect.DeepEqual(first, read) {
		t.Fatalf("qualified wait read changed winner: first=%+v read=%+v", first, read)
	}
}

func TestLifetimeWorkerWaitRejectsWrongIdentityAndUnqualifiedCooperative(t *testing.T) {
	t.Run("cooperative-without-worker", func(t *testing.T) {
		td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, false)
		if worker != nil || workerDigest != "" {
			t.Fatal("cooperative fixture unexpectedly persisted a worker")
		}
		_, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
			ChildPID: 123,
			Result:   task.LifetimeWorkerWaitSuccess,
			ExitCode: taskdirIntPointer(0),
		})
		if !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("unqualified cooperative wait error = %v, want identity mismatch", err)
		}
	})

	t.Run("wrong-worker-digest", func(t *testing.T) {
		td, worker, _ := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		_, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
			WorkerSHA256: task.ComputeSHA256([]byte("other-worker")),
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     taskdirIntPointer(0),
		})
		if !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong worker digest error = %v, want identity mismatch", err)
		}
	})

	t.Run("wrong-pid", func(t *testing.T) {
		td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		_, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID + 1,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     taskdirIntPointer(0),
		})
		if !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong worker PID error = %v, want identity mismatch", err)
		}
	})

	t.Run("wrong-registration", func(t *testing.T) {
		td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		_, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     taskdirIntPointer(0),
		})
		must(t, err)
		path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
		data, err := os.ReadFile(path)
		must(t, err)
		var record task.LifetimeWorkerWaitRecord
		must(t, task.DecodeStrict(data, &record))
		record.RegistrationSHA256 = task.ComputeSHA256([]byte("other-registration"))
		data, err = task.MarshalCanonical(record)
		must(t, err)
		must(t, os.WriteFile(path, data, 0o600))
		if _, err = td.ReadLifetimeWorkerWait(); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong registration read error = %v, want identity mismatch", err)
		}
	})
}

func TestLifetimeWorkerWaitCreateOnceRejectsConflictsAndMalformedWinner(t *testing.T) {
	td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
	first, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     taskdirIntPointer(0),
		ObservedAt:   time.Date(2026, time.October, 11, 11, 31, 0, 0, time.UTC),
	})
	must(t, err)
	replay, err := td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     taskdirIntPointer(0),
		ObservedAt:   time.Date(2026, time.October, 11, 12, 31, 0, 0, time.UTC),
	})
	must(t, err)
	if !reflect.DeepEqual(first, replay) {
		t.Fatalf("exact replay changed winner: first=%+v replay=%+v", first, replay)
	}
	if _, err = td.RecordLifetimeWorkerWait(LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitExitError,
		ExitCode:     taskdirIntPointer(-1),
	}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting wait error = %v, want identity mismatch", err)
	}

	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
	data, err := os.ReadFile(path)
	must(t, err)
	var malformed task.LifetimeWorkerWaitRecord
	must(t, task.DecodeStrict(data, &malformed))
	malformed.Result = "not-a-result"
	data, err = task.MarshalCanonical(malformed)
	must(t, err)
	must(t, os.WriteFile(path, data, 0o600))
	if _, err = td.ReadLifetimeWorkerWait(); !errors.Is(err, task.ErrInvalidEnum) {
		t.Fatalf("malformed wait read error = %v, want invalid enum", err)
	}
}

func TestLifetimeWorkerWaitAndRegistrationReplayReacknowledgeDurability(t *testing.T) {
	t.Run("existing-registration", func(t *testing.T) {
		store := testStore(t)
		td := preparedTask(t, store)
		deadline := time.Date(2026, time.October, 11, 12, 0, 0, 0, time.UTC)
		intent := LifetimeIntentFacts{
			ScopeKind: task.LifetimeScopeCgroupV2,
			Assurance: task.LifetimeAssuranceExact,
			ScopePlan: "cgroup-plan-replay",
			BootID:    "boot-replay",
			Deadline:  deadline,
		}
		facts := LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeCgroupV2,
			Assurance: task.LifetimeAssuranceExact,
			ScopeID:   "/sys/fs/cgroup/replay",
			BootID:    "boot-replay",
			Deadline:  deadline,
		}
		recordTaskLifetimeIntent(t, td, intent)
		mustRecordLifetimeRegistration(t, td, facts)
		fault := &orderedFault{target: lifetimeRegistrationName, fail: "post-link"}
		store.SetFaultInjector(fault)
		if _, err := td.RecordLifetimeRegistration(facts); !errors.Is(err, task.ErrUncertainDurability) {
			t.Fatalf("existing registration replay error = %v, want uncertain durability", err)
		}
		if !containsEvent(fault.events, "post-link:"+lifetimeRegistrationName) {
			t.Fatalf("existing registration replay omitted post-link acknowledgement: %v", fault.events)
		}
		store.SetFaultInjector(nil)
		mustRecordLifetimeRegistration(t, td, facts)
	})

	t.Run("wait-post-link", func(t *testing.T) {
		td, worker, workerDigest := prepareTaskLifetimeWorkerWait(t, task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative, true)
		observedAt := time.Date(2026, time.October, 11, 11, 32, 0, 0, time.UTC)
		facts := LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     taskdirIntPointer(0),
			ObservedAt:   observedAt,
		}
		fault := &orderedFault{target: lifetimeWorkerWaitName, fail: "post-link"}
		td.store.SetFaultInjector(fault)
		if _, err := td.RecordLifetimeWorkerWait(facts); !errors.Is(err, task.ErrUncertainDurability) {
			t.Fatalf("wait post-link error = %v, want uncertain durability", err)
		}
		if _, err := os.Stat(filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)); err != nil {
			t.Fatalf("wait winner disappeared after ambiguous publication: %v", err)
		}
		td.store.SetFaultInjector(nil)
		replayed, err := td.RecordLifetimeWorkerWait(facts)
		must(t, err)
		if replayed.ObservedAt != observedAt.Format(time.RFC3339Nano) {
			t.Fatalf("wait replay changed timestamp: got=%q want=%q", replayed.ObservedAt, observedAt.Format(time.RFC3339Nano))
		}
	})
}

func prepareTaskLifetimeWorkerWait(t *testing.T, scopeKind, assurance string, withWorker bool) (*TaskDir, *task.LifetimeWorkerRecord, string) {
	t.Helper()
	store := testStore(t)
	td := preparedTask(t, store)
	deadline := time.Date(2026, time.October, 11, 12, 0, 0, 0, time.UTC)
	recordTaskLifetimeIntent(t, td, LifetimeIntentFacts{
		ScopeKind: scopeKind,
		Assurance: assurance,
		ScopePlan: "lifetime-worker-wait-plan",
		BootID:    "lifetime-worker-wait-boot",
		Deadline:  deadline,
	})
	mustRecordLifetimeRegistration(t, td, LifetimeRegistrationFacts{
		ScopeKind: scopeKind,
		Assurance: assurance,
		ScopeID:   "lifetime-worker-wait-scope",
		BootID:    "lifetime-worker-wait-boot",
		Deadline:  deadline,
	})
	if !withWorker {
		return td, nil, ""
	}
	worker, err := td.RecordLifetimeWorker(LifetimeWorkerFacts{
		PID:          123,
		ProcessStart: "lifetime-worker-wait-start",
		BootID:       "lifetime-worker-wait-boot",
	})
	must(t, err)
	digest, err := td.LifetimeWorkerDigest()
	must(t, err)
	return td, worker, digest
}

func taskdirIntPointer(value int) *int {
	return &value
}

func mustRecordLifetimeRegistration(t *testing.T, td *TaskDir, facts LifetimeRegistrationFacts) {
	t.Helper()
	_, err := td.RecordLifetimeRegistration(facts)
	must(t, err)
}
