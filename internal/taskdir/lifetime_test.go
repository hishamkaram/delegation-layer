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

type lifetimeContextWriterFacts struct {
	intent       LifetimeIntentFacts
	registration LifetimeRegistrationFacts
	worker       LifetimeWorkerFacts
	wait         LifetimeWorkerWaitFacts
}

func lifetimeContextWriters() []lifetimeContextWriter {
	facts := newLifetimeContextWriterFacts()
	writers := lifetimeContextIntentWriters(facts)
	return append(writers, lifetimeContextWorkerWriters(facts)...)
}

func newLifetimeContextWriterFacts() lifetimeContextWriterFacts {
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
	return lifetimeContextWriterFacts{intent: intent, registration: registration, worker: worker, wait: wait}
}

func lifetimeContextIntentWriters(facts lifetimeContextWriterFacts) []lifetimeContextWriter {
	return []lifetimeContextWriter{
		{
			name: "intent",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeIntentName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeIntentContext(ctx, facts.intent)
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
				recordTaskLifetimeIntent(t, td, facts.intent)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeRegistrationContext(ctx, facts.registration)
				return err
			},
			read: func(ctx context.Context, td *TaskDir) error {
				_, err := td.ReadLifetimeRegistrationContext(ctx)
				return err
			},
		},
	}
}

func lifetimeContextWorkerWriters(facts lifetimeContextWriterFacts) []lifetimeContextWriter {
	return []lifetimeContextWriter{
		{
			name: "worker",
			setup: func(t *testing.T) (*TaskDir, string) {
				t.Helper()
				td := preparedTask(t, testStore(t))
				recordTaskLifetimeIntent(t, td, facts.intent)
				mustRecordLifetimeRegistration(t, td, facts.registration)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeWorkerContext(ctx, facts.worker)
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
				recordTaskLifetimeIntent(t, td, facts.intent)
				mustRecordLifetimeRegistration(t, td, facts.registration)
				return td, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
			},
			write: func(ctx context.Context, td *TaskDir) error {
				_, err := td.RecordLifetimeWorkerWaitContext(ctx, facts.wait)
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
