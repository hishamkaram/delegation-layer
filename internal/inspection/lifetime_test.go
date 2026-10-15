package inspection

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func openInspectionLifetimeTest(t *testing.T) (*taskdir.Store, *Operation) {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Now().UTC())
	mustInspection(t, err)
	owner, err := op.AcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, owner.Release()) })
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	return store, op
}

func inspectionLifetimeRegistrationFacts(op *Operation) taskdir.LifetimeRegistrationFacts {
	return taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "inspection-process-group",
		BootID:    "inspection-boot",
		Deadline:  op.Deadline(),
	}
}

func putInspectionLifetimeLaunch(t *testing.T, op *Operation, registrationDigest string, sequence uint64) {
	putInspectionLifetimeLaunchAt(t, op, registrationDigest, sequence, sequence)
}

func putInspectionLifetimeLaunchAt(t *testing.T, op *Operation, registrationDigest string, fileSequence, recordSequence uint64) {
	t.Helper()
	request := op.Request()
	record := task.LifetimeLaunchRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             request.RootID,
		TaskID:             request.TaskID,
		RegistrationSHA256: registrationDigest,
		Sequence:           recordSequence,
		PID:                1000 + int(recordSequence),
		ProcessStart:       fmt.Sprintf("inspection-start-%d", recordSequence),
		PGID:               1000 + int(recordSequence),
		StartedAt:          request.CreatedAt,
	}
	created, err := op.control.Put(inspectionLifetimeLaunchName(fileSequence), record)
	mustInspection(t, err)
	if !created {
		t.Fatalf("lifetime launch %d was not created", fileSequence)
	}
}

func TestInspectionLifetimeRecordsBindOwnerAndFenceTermination(t *testing.T) {
	_, op := openInspectionLifetimeTest(t)
	facts := inspectionLifetimeRegistrationFacts(op)
	registration := recordInspectionLifetimeForTest(t, op, facts)
	assertInspectionLifetimeRegistration(t, op, facts, registration)
	recordInspectionLifetimeLaunchesForTest(t, op)
	assertInspectionLifetimeTermination(t, op)
}

func TestInspectionPresentTerminationWithMissingRegistrationIsNotAbsent(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	facts := inspectionLifetimeRegistrationFacts(op)
	recordInspectionLifetimeForTest(t, op, facts)
	recordInspectionLifetimeLaunchesForTest(t, op)
	_, err := op.RecordLifetimeTermination(task.LifetimeTerminationCompleted)
	mustInspection(t, err)
	registrationPath := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeRegistrationName)
	mustInspection(t, os.Remove(registrationPath))

	if _, err = op.ReadLifetimeTerminationContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("termination read error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestInspectionLifetimeRegistrationPostLinkErrorRequiresExactReplay(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	facts := inspectionLifetimeRegistrationFacts(op)
	_, err := op.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "inspection-scope-plan-post-link",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	})
	mustInspection(t, err)

	store.SetFaultInjector(&inspectionLifetimePostLinkFault{})
	if _, err = op.RecordLifetimeRegistration(facts); err == nil {
		t.Fatal("post-link registration fault was not reported")
	}
	store.SetFaultInjector(nil)
	winner, err := op.ReadLifetimeRegistration()
	mustInspection(t, err)
	replayed, err := op.RecordLifetimeRegistration(facts)
	mustInspection(t, err)
	if replayed.ScopeID != winner.ScopeID || replayed.CreatedAt != winner.CreatedAt {
		t.Fatalf("registration replay changed winner: first=%+v replay=%+v", winner, replayed)
	}
}

func TestInspectionLifetimeWorkerPostLinkErrorRequiresExactReplay(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	registration := inspectionLifetimeRegistrationFacts(op)
	recordInspectionLifetimeForTest(t, op, registration)
	facts := taskdir.LifetimeWorkerFacts{PID: 404, ProcessStart: "inspection-worker-post-link", BootID: registration.BootID}
	store.SetFaultInjector(&inspectionLifetimeWorkerPostLinkFault{})
	if _, err := op.RecordLifetimeWorker(facts); !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("worker post-link error = %v, want uncertain durability", err)
	}
	winner, err := op.ReadLifetimeWorker()
	mustInspection(t, err)
	store.SetFaultInjector(nil)
	replayed, err := op.RecordLifetimeWorker(facts)
	mustInspection(t, err)
	if !reflect.DeepEqual(replayed, winner) {
		t.Fatalf("worker replay changed winner: first=%+v replay=%+v", winner, replayed)
	}
}

type inspectionLifetimePostLinkFault struct{ taskdir.BaseFaultInjector }

func (*inspectionLifetimePostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == inspectionLifetimeRegistrationName {
		return errors.New("injected inspection lifetime post-link failure")
	}
	return nil
}

type inspectionLifetimeWorkerPostLinkFault struct{ taskdir.BaseFaultInjector }

func (*inspectionLifetimeWorkerPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == inspectionLifetimeWorkerName {
		return errors.New("injected inspection lifetime worker post-link failure")
	}
	return nil
}

func recordInspectionLifetimeForTest(t *testing.T, op *Operation, facts taskdir.LifetimeRegistrationFacts) *task.LifetimeRegistrationRecord {
	t.Helper()
	_, err := op.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "inspection-scope-plan-v1",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	})
	mustInspection(t, err)
	registration, err := op.RecordLifetimeRegistration(facts)
	mustInspection(t, err)
	return registration
}

func assertInspectionLifetimeRegistration(t *testing.T, op *Operation, facts taskdir.LifetimeRegistrationFacts, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	if registration.OwnerKind != task.LifetimeOwnerInspection || registration.OwnerSHA256 != op.Digest() {
		t.Fatalf("registration owner = %q/%q, want inspection/%q", registration.OwnerKind, registration.OwnerSHA256, op.Digest())
	}
	request := op.Request()
	if registration.RootID != request.RootID || registration.TaskID != request.TaskID || registration.Deadline != request.Deadline {
		t.Fatalf("registration identity/deadline = %+v, want root=%q task=%q deadline=%q", registration, request.RootID, request.TaskID, request.Deadline)
	}

	if _, err := op.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopeID:   "different-scope",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting registration error = %v, want identity mismatch", err)
	}
}

func recordInspectionLifetimeLaunchesForTest(t *testing.T, op *Operation) {
	t.Helper()
	request := op.Request()
	launch, err := op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: 1, PID: 1001, ProcessStart: "inspection-start-1", PGID: 1001})
	mustInspection(t, err)
	if launch.RegistrationSHA256 == "" || launch.RootID != request.RootID || launch.TaskID != request.TaskID {
		t.Fatalf("launch binding = %+v", launch)
	}
	_, err = op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: 2, PID: 1002, ProcessStart: "inspection-start-2", PGID: 1002})
	mustInspection(t, err)
	launches, err := op.ReadLifetimeLaunches()
	mustInspection(t, err)
	if len(launches) != 2 || launches[0].Sequence != 1 || launches[1].Sequence != 2 {
		t.Fatalf("launch set = %+v", launches)
	}
}

func assertInspectionLifetimeTermination(t *testing.T, op *Operation) {
	t.Helper()
	termination, err := op.RecordLifetimeTermination(task.LifetimeTerminationBudget)
	mustInspection(t, err)
	if termination.LaunchCount != 2 || !termination.ScopeEmpty || termination.Cause != task.LifetimeTerminationBudget {
		t.Fatalf("termination = %+v", termination)
	}
	replayed, err := op.RecordLifetimeTermination(task.LifetimeTerminationBudget)
	mustInspection(t, err)
	if *replayed != *termination {
		t.Fatalf("idempotent termination changed winner: first=%+v replay=%+v", termination, replayed)
	}
	if _, err = op.RecordLifetimeTermination(task.LifetimeTerminationCompleted); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting termination error = %v, want identity mismatch", err)
	}
	if _, err = op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: 3, PID: 1003, ProcessStart: "inspection-start-3", PGID: 1003}); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("launch after termination error = %v, want terminal task", err)
	}
	has, err := op.HasTerminatedLifetime(task.LifetimeTerminationBudget)
	mustInspection(t, err)
	if !has {
		t.Fatal("budget termination was not authoritative")
	}
	has, err = op.HasTerminatedLifetime(task.LifetimeTerminationCompleted)
	mustInspection(t, err)
	if has {
		t.Fatal("termination reported for a different cause")
	}
}

func TestInspectionLifetimeRegistrationReadBindsOwner(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))

	path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeRegistrationName)
	data, err := os.ReadFile(path)
	mustInspection(t, err)
	var record task.LifetimeRegistrationRecord
	mustInspection(t, task.DecodeStrict(data, &record))
	record.OwnerSHA256 = strings.Repeat("f", 64)
	data, err = task.MarshalCanonical(record)
	mustInspection(t, err)
	mustInspection(t, os.WriteFile(path, data, 0o600))
	if _, err = op.ReadLifetimeRegistration(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("owner mismatch read error = %v, want identity mismatch", err)
	}
}

func TestInspectionLifetimeReadsRequireCanonicalWinners(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))

	path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeRegistrationName)
	data, err := os.ReadFile(path)
	mustInspection(t, err)
	mustInspection(t, os.WriteFile(path, append(data, ' '), 0o600))
	if _, err = op.ReadLifetimeRegistration(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("noncanonical registration read error = %v, want evidence fault", err)
	}
}

func TestInspectionLifetimeLaunchesRejectGapsAndOverflow(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
		var err error
		registrationDigest, err := op.LifetimeRegistrationDigest()
		mustInspection(t, err)
		putInspectionLifetimeLaunch(t, op, registrationDigest, 2)
		if _, err = op.ReadLifetimeLaunches(); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("gap read error = %v, want identity mismatch", err)
		}
		if _, err = op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: 3, PID: 1003, ProcessStart: "inspection-start-3", PGID: 1003}); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("launch after gap error = %v, want identity mismatch", err)
		}
	})

	t.Run("filename-sequence-conflict", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
		var err error
		registrationDigest, err := op.LifetimeRegistrationDigest()
		mustInspection(t, err)
		putInspectionLifetimeLaunchAt(t, op, registrationDigest, 1, 2)
		if _, err = op.ReadLifetimeLaunches(); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("filename/sequence conflict error = %v, want identity mismatch", err)
		}
	})

	t.Run("overflow", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
		var err error
		registrationDigest, err := op.LifetimeRegistrationDigest()
		mustInspection(t, err)
		for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches+1; sequence++ {
			putInspectionLifetimeLaunch(t, op, registrationDigest, sequence)
		}
		if _, err = op.ReadLifetimeLaunches(); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("overflow read error = %v, want evidence fault", err)
		}
		if _, err = op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: task.MaxLifetimeLaunches + 1, PID: 1065, ProcessStart: "inspection-start-65", PGID: 1065}); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("overflow write error = %v, want evidence fault", err)
		}
	})
}

func TestInspectionLifetimeTerminationFencesChangedLaunchSet(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
	var err error
	_, err = op.RecordLifetimeLaunch(taskdir.LifetimeLaunchFacts{Sequence: 1, PID: 1001, ProcessStart: "inspection-start-1", PGID: 1001})
	mustInspection(t, err)
	_, err = op.RecordLifetimeTermination(task.LifetimeTerminationRecovery)
	mustInspection(t, err)

	registrationDigest, err := op.LifetimeRegistrationDigest()
	mustInspection(t, err)
	putInspectionLifetimeLaunch(t, op, registrationDigest, 2)
	if _, err = op.ReadLifetimeTermination(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("fenced termination read error = %v, want identity mismatch", err)
	}
	if _, err = os.Stat(filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeTerminationName)); err != nil {
		t.Fatalf("termination record disappeared: %v", err)
	}
}

func TestInspectionLifetimeIntentAndWorkerRecords(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	facts := inspectionLifetimeRegistrationFacts(op)
	intent, err := op.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "inspection-scope-plan-v1",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	})
	mustInspection(t, err)
	replayedIntent, err := op.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "inspection-scope-plan-v1",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	})
	mustInspection(t, err)
	if *replayedIntent != *intent {
		t.Fatalf("intent replay changed winner: first=%+v replay=%+v", intent, replayedIntent)
	}
	if _, err = op.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: facts.ScopeKind,
		Assurance: facts.Assurance,
		ScopePlan: "different-plan",
		BootID:    facts.BootID,
		Deadline:  facts.Deadline,
	}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting intent error = %v, want identity mismatch", err)
	}
	if _, err = op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-start-1", BootID: facts.BootID}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker before registration error = %v, want missing registration", err)
	}
	registration, err := op.RecordLifetimeRegistration(facts)
	mustInspection(t, err)
	worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-start-1", BootID: facts.BootID})
	mustInspection(t, err)
	if worker.RegistrationSHA256 == "" || worker.RegistrationSHA256 != digestInspectionLifetimeRecord(t, registration) {
		t.Fatalf("worker registration binding = %+v", worker)
	}
	replayedWorker, err := op.ReadLifetimeWorkerContext(t.Context())
	mustInspection(t, err)
	if *replayedWorker != *worker {
		t.Fatalf("worker read changed winner: first=%+v replay=%+v", worker, replayedWorker)
	}
	if _, err = op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1002, ProcessStart: "inspection-start-2", BootID: facts.BootID}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting worker error = %v, want identity mismatch", err)
	}
	path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerName)
	data, err := os.ReadFile(path)
	mustInspection(t, err)
	var tampered task.LifetimeWorkerRecord
	mustInspection(t, task.DecodeStrict(data, &tampered))
	tampered.RegistrationSHA256 = task.ComputeSHA256([]byte("other-registration"))
	data, err = task.MarshalCanonical(tampered)
	mustInspection(t, err)
	mustInspection(t, os.WriteFile(path, data, 0o600))
	if _, err = op.ReadLifetimeWorker(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("tampered worker digest error = %v, want identity mismatch", err)
	}
}

func TestInspectionLifetimeWorkerWaitRecordsValidResultsAndQualification(t *testing.T) {
	observedAt := time.Date(2026, time.October, 11, 11, 30, 0, 0, time.UTC)
	t.Run("exact-unqualified", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		facts := taskdir.LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeCgroupV2,
			Assurance: task.LifetimeAssuranceExact,
			ScopeID:   "/sys/fs/cgroup/inspection-wait",
			BootID:    "inspection-exact-boot",
			Deadline:  op.Deadline(),
		}
		recordInspectionLifetimeForTest(t, op, facts)
		first, err := op.RecordLifetimeWorkerWaitContext(t.Context(), taskdir.LifetimeWorkerWaitFacts{
			ChildPID:   701,
			Result:     task.LifetimeWorkerWaitSuccess,
			ExitCode:   inspectionIntPointer(0),
			ObservedAt: observedAt,
		})
		mustInspection(t, err)
		if task.LifetimeWorkerWaitIsQualified(first) || first.WorkerSHA256 != "" {
			t.Fatalf("exact wait unexpectedly qualified: %+v", first)
		}
		read, err := op.ReadLifetimeWorkerWait()
		mustInspection(t, err)
		if !reflect.DeepEqual(first, read) {
			t.Fatalf("unqualified wait read changed winner: first=%+v read=%+v", first, read)
		}
	})
	t.Run("cooperative-not-started", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
		first, err := op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			Result: task.LifetimeWorkerWaitNotStarted, ObservedAt: observedAt,
		})
		mustInspection(t, err)
		if !task.LifetimeWorkerWaitProvesNotStarted(first) {
			t.Fatalf("worker disposition = %+v, want not-started", first)
		}
		read, err := op.ReadLifetimeWorkerWaitContext(t.Context())
		mustInspection(t, err)
		if !reflect.DeepEqual(first, read) {
			t.Fatalf("not-started read changed winner: first=%+v read=%+v", first, read)
		}
	})

	for _, test := range []struct {
		name     string
		result   task.LifetimeWorkerWaitResult
		exitCode *int
	}{
		{name: "success", result: task.LifetimeWorkerWaitSuccess, exitCode: inspectionIntPointer(0)},
		{name: "exit-error", result: task.LifetimeWorkerWaitExitError, exitCode: inspectionIntPointer(-1)},
		{name: "wait-error", result: task.LifetimeWorkerWaitError},
	} {
		t.Run("qualified-"+test.name, func(t *testing.T) {
			store, op := openInspectionLifetimeTest(t)
			registration := inspectionLifetimeRegistrationFacts(op)
			recordInspectionLifetimeForTest(t, op, registration)
			worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
				PID:          1001,
				ProcessStart: "inspection-wait-start",
				BootID:       registration.BootID,
			})
			mustInspection(t, err)
			workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
			first, err := op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
				WorkerSHA256: workerDigest,
				ChildPID:     worker.PID,
				Result:       test.result,
				ExitCode:     test.exitCode,
				ObservedAt:   observedAt,
			})
			mustInspection(t, err)
			if !task.LifetimeWorkerWaitIsQualified(first) || first.WorkerSHA256 != workerDigest {
				t.Fatalf("qualified wait binding = %+v, want worker %q", first, workerDigest)
			}
			read, err := op.ReadLifetimeWorkerWaitContext(t.Context())
			mustInspection(t, err)
			if !reflect.DeepEqual(first, read) {
				t.Fatalf("qualified wait read changed winner: first=%+v read=%+v", first, read)
			}
			if _, err = os.Stat(filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerWaitName)); err != nil {
				t.Fatalf("worker wait record missing: %v", err)
			}
		})
	}
}

func TestInspectionLifetimeWorkerWaitRejectsWrongIdentityAndConflicts(t *testing.T) {
	t.Run("cooperative-without-worker", func(t *testing.T) {
		_, op := openInspectionLifetimeTest(t)
		facts := inspectionLifetimeRegistrationFacts(op)
		recordInspectionLifetimeForTest(t, op, facts)
		_, err := op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			ChildPID: 123,
			Result:   task.LifetimeWorkerWaitSuccess,
			ExitCode: inspectionIntPointer(0),
		})
		if !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("unqualified cooperative wait error = %v, want identity mismatch", err)
		}
	})

	t.Run("wrong-worker-and-pid", func(t *testing.T) {
		store, op := openInspectionLifetimeTest(t)
		facts := inspectionLifetimeRegistrationFacts(op)
		recordInspectionLifetimeForTest(t, op, facts)
		worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-wait-start", BootID: facts.BootID})
		mustInspection(t, err)
		workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
		waitFacts := taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: task.ComputeSHA256([]byte("other-worker")),
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     inspectionIntPointer(0),
		}
		if _, err = op.RecordLifetimeWorkerWait(waitFacts); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong worker digest error = %v, want identity mismatch", err)
		}
		waitFacts.WorkerSHA256 = workerDigest
		waitFacts.ChildPID++
		if _, err = op.RecordLifetimeWorkerWait(waitFacts); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong worker PID error = %v, want identity mismatch", err)
		}
		waitFacts.ChildPID = worker.PID
		_, err = op.RecordLifetimeWorkerWait(waitFacts)
		mustInspection(t, err)

		path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerWaitName)
		data, err := os.ReadFile(path)
		mustInspection(t, err)
		var record task.LifetimeWorkerWaitRecord
		mustInspection(t, task.DecodeStrict(data, &record))
		record.RegistrationSHA256 = task.ComputeSHA256([]byte("other-registration"))
		data, err = task.MarshalCanonical(record)
		mustInspection(t, err)
		mustInspection(t, os.WriteFile(path, data, 0o600))
		if _, err = op.ReadLifetimeWorkerWait(); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("wrong registration read error = %v, want identity mismatch", err)
		}
	})

	t.Run("conflicting-replay-and-malformed-winner", func(t *testing.T) {
		store, op := openInspectionLifetimeTest(t)
		facts := inspectionLifetimeRegistrationFacts(op)
		recordInspectionLifetimeForTest(t, op, facts)
		worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-wait-start", BootID: facts.BootID})
		mustInspection(t, err)
		workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
		firstFacts := taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     inspectionIntPointer(0),
			ObservedAt:   time.Date(2026, time.October, 11, 11, 31, 0, 0, time.UTC),
		}
		first, err := op.RecordLifetimeWorkerWait(firstFacts)
		mustInspection(t, err)
		replayFacts := firstFacts
		replayFacts.ObservedAt = time.Date(2026, time.October, 11, 12, 31, 0, 0, time.UTC)
		replay, err := op.RecordLifetimeWorkerWaitContext(t.Context(), replayFacts)
		mustInspection(t, err)
		if !reflect.DeepEqual(first, replay) {
			t.Fatalf("exact replay changed winner: first=%+v replay=%+v", first, replay)
		}
		conflict := firstFacts
		conflict.Result = task.LifetimeWorkerWaitExitError
		conflict.ExitCode = inspectionIntPointer(-1)
		if _, err = op.RecordLifetimeWorkerWait(conflict); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("conflicting wait error = %v, want identity mismatch", err)
		}

		path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerWaitName)
		data, err := os.ReadFile(path)
		mustInspection(t, err)
		var malformed task.LifetimeWorkerWaitRecord
		mustInspection(t, task.DecodeStrict(data, &malformed))
		malformed.Result = "not-a-result"
		data, err = task.MarshalCanonical(malformed)
		mustInspection(t, err)
		mustInspection(t, os.WriteFile(path, data, 0o600))
		if _, err = op.ReadLifetimeWorkerWaitContext(t.Context()); !errors.Is(err, task.ErrInvalidEnum) {
			t.Fatalf("malformed wait read error = %v, want invalid enum", err)
		}
	})
}

func TestInspectionLifetimeWorkerWaitReplayReacknowledgesAmbiguousWinner(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	facts := inspectionLifetimeRegistrationFacts(op)
	recordInspectionLifetimeForTest(t, op, facts)
	worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-wait-start", BootID: facts.BootID})
	mustInspection(t, err)
	workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
	waitFacts := taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: workerDigest,
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     inspectionIntPointer(0),
		ObservedAt:   time.Date(2026, time.October, 11, 11, 32, 0, 0, time.UTC),
	}
	store.SetFaultInjector(&inspectionLifetimeWorkerWaitPostLinkFault{})
	if _, err = op.RecordLifetimeWorkerWait(waitFacts); !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("wait post-link error = %v, want uncertain durability", err)
	}
	store.SetFaultInjector(nil)
	if _, err = op.ReadLifetimeWorkerWait(); err != nil {
		t.Fatalf("ambiguous wait winner was not readable: %v", err)
	}
	replayed, err := op.RecordLifetimeWorkerWaitContext(t.Context(), waitFacts)
	mustInspection(t, err)
	if replayed.ObservedAt != waitFacts.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatalf("wait replay changed timestamp: got=%q want=%q", replayed.ObservedAt, waitFacts.ObservedAt.Format(time.RFC3339Nano))
	}
}

func TestInspectionLifetimeWorkerWaitReadRevalidatesUpstreamDigests(t *testing.T) {
	t.Run("registration-digest", func(t *testing.T) {
		store, op := openInspectionLifetimeTest(t)
		facts := inspectionLifetimeRegistrationFacts(op)
		recordInspectionLifetimeForTest(t, op, facts)
		worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-wait-start", BootID: facts.BootID})
		mustInspection(t, err)
		workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
		_, err = op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     inspectionIntPointer(0),
		})
		mustInspection(t, err)

		path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeRegistrationName)
		data, err := os.ReadFile(path)
		mustInspection(t, err)
		var registration task.LifetimeRegistrationRecord
		mustInspection(t, task.DecodeStrict(data, &registration))
		registration.ScopeID = "inspection-process-group-mutated"
		data, err = task.MarshalCanonical(registration)
		mustInspection(t, err)
		mustInspection(t, os.WriteFile(path, data, 0o600))
		if _, err = op.ReadLifetimeWorkerWait(); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("upstream registration mutation error = %v, want identity mismatch", err)
		}
	})

	t.Run("worker-digest", func(t *testing.T) {
		store, op := openInspectionLifetimeTest(t)
		facts := inspectionLifetimeRegistrationFacts(op)
		recordInspectionLifetimeForTest(t, op, facts)
		worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1001, ProcessStart: "inspection-wait-start", BootID: facts.BootID})
		mustInspection(t, err)
		workerDigest := digestInspectionLifetimeWorkerRecord(t, worker)
		_, err = op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: workerDigest,
			ChildPID:     worker.PID,
			Result:       task.LifetimeWorkerWaitSuccess,
			ExitCode:     inspectionIntPointer(0),
		})
		mustInspection(t, err)

		path := filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerName)
		data, err := os.ReadFile(path)
		mustInspection(t, err)
		var mutated task.LifetimeWorkerRecord
		mustInspection(t, task.DecodeStrict(data, &mutated))
		mutated.ProcessStart = "inspection-wait-start-mutated"
		data, err = task.MarshalCanonical(mutated)
		mustInspection(t, err)
		mustInspection(t, os.WriteFile(path, data, 0o600))
		if _, err = op.ReadLifetimeWorkerWaitContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("upstream worker mutation error = %v, want identity mismatch", err)
		}
	})
}

type inspectionLifetimeWorkerWaitPostLinkFault struct{ taskdir.BaseFaultInjector }

func (*inspectionLifetimeWorkerWaitPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == inspectionLifetimeWorkerWaitName {
		return errors.New("injected inspection lifetime worker wait post-link failure")
	}
	return nil
}

func digestInspectionLifetimeRecord(t *testing.T, record *task.LifetimeRegistrationRecord) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	mustInspection(t, err)
	return task.ComputeSHA256(data)
}

func digestInspectionLifetimeWorkerRecord(t *testing.T, record *task.LifetimeWorkerRecord) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	mustInspection(t, err)
	return task.ComputeSHA256(data)
}

func inspectionIntPointer(value int) *int {
	return &value
}

func TestInspectionLifetimeRegistrationRequiresIntentForNewWrites(t *testing.T) {
	_, op := openInspectionLifetimeTest(t)
	if _, err := op.RecordLifetimeRegistration(inspectionLifetimeRegistrationFacts(op)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration without intent error = %v, want missing intent", err)
	}
}

func TestInspectionLifetimeLegacyRegistrationReadRemainsCompatible(t *testing.T) {
	store, op := openInspectionLifetimeTest(t)
	request := op.Request()
	facts := inspectionLifetimeRegistrationFacts(op)
	legacy := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        request.RootID,
		TaskID:        request.TaskID,
		OwnerKind:     task.LifetimeOwnerInspection,
		OwnerSHA256:   op.Digest(),
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopeID:       facts.ScopeID,
		BootID:        facts.BootID,
		Deadline:      request.Deadline,
		CreatedAt:     request.CreatedAt,
	}
	mustInspection(t, task.ValidateLifetimeRegistrationRecord(&legacy))
	created, err := op.control.Put(inspectionLifetimeRegistrationName, legacy)
	mustInspection(t, err)
	if !created {
		t.Fatal("legacy registration was not created")
	}
	read, err := op.ReadLifetimeRegistrationContext(t.Context())
	mustInspection(t, err)
	if *read != legacy {
		t.Fatalf("legacy registration changed on read: got=%+v want=%+v", read, legacy)
	}
	if _, err = os.Stat(filepath.Join(store.Root, "inspections", request.TaskID, inspectionLifetimeIntentName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy read unexpectedly created intent: %v", err)
	}
}
