package inspection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRuntimeCapabilityMetadataFitsInspectionJournalBudget(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.DefinitionRevision = provider.RuntimeInspectionRevision
	binding.CapabilitySHA256 = strings.Repeat("a", 64)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })

	facts, err := provider.EncodeInspectionFacts(provider.RuntimeFacts{
		Executable:       "/usr/bin/provider",
		Version:          strings.Repeat("v", 4000),
		SHA256:           strings.Repeat("b", 64),
		CapabilitySHA256: strings.Repeat("c", 64),
	}, nil)
	mustInspection(t, err)
	if len(facts) <= maxProjectedFacts || len(facts) > maxRuntimeFacts {
		t.Fatalf("runtime facts length=%d, want (%d,%d]", len(facts), maxProjectedFacts, maxRuntimeFacts)
	}
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, facts, time.Unix(102, 0)))
	mustInspection(t, op.RecordWorkerSuccess(42))
	stored, err := op.ReadProof()
	mustInspection(t, err)
	if string(stored) != string(facts) {
		t.Fatal("runtime capability facts changed across journal persistence")
	}
}

func TestCompletedInspectionRequiresIndependentWorkerSuccess(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, []byte(`{"eligible":true}`), time.Unix(102, 0)))
	if _, err = op.ReadProof(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("local completion qualified without worker success")
	}
	if err = op.RecordWorkerSuccess(43); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("wrong worker qualified")
	}
	mustInspection(t, op.RecordWorkerSuccess(42))
	facts, err := op.ReadProof()
	mustInspection(t, err)
	if string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("unexpected facts: %s", facts)
	}
	mustInspection(t, op.RecordWorkerSuccess(42))
}

func TestFailedInspectionCannotBePromotedByWorkerExit(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultUnavailable, nil, time.Unix(102, 0)))
	if err = op.RecordWorkerSuccess(42); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("failed inspection promoted")
	}
}

func TestCanceledWorkerSuccessContextCannotPublishAuthority(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, []byte(`{"eligible":true}`), time.Unix(102, 0)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = op.RecordWorkerSuccessContext(ctx, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled worker-success context returned %v", err)
	}
	var observation WorkerObservationRecord
	if err = op.control.Read(workerObservationRecordName, &observation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled worker-success context published authority: %v", err)
	}
}

func TestCanceledProofContextDoesNotReadProof(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.Complete(ResultEligible, []byte(`{"eligible":true}`), time.Unix(102, 0)))
	mustInspection(t, op.RecordWorkerSuccess(42))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = op.ReadProofContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled proof context returned %v", err)
	}
}

func TestFailedWorkerObservationNeverGrantsAdmission(t *testing.T) {
	for _, reason := range []ResultReason{ResultEligible, ResultUnavailable} {
		t.Run(string(reason), func(t *testing.T) {
			op := evidenceOperation(t)
			_, err := op.ClaimStart(time.Unix(101, 0))
			mustInspection(t, err)
			mustInspection(t, op.RecordReceipt(42))
			var facts []byte
			if reason == ResultEligible {
				facts = []byte(`{"eligible":true}`)
			}
			mustInspection(t, op.Complete(reason, facts, time.Unix(102, 0)))
			if _, _, err = op.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("local completion supplied independent exit evidence: %v", err)
			}
			if err = op.RecordWorkerExitContext(t.Context(), 43, false); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("wrong worker supplied terminal evidence: %v", err)
			}
			mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, false))
			mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, false))
			result, succeeded, readErr := op.ReadWorkerResultContext(t.Context())
			mustInspection(t, readErr)
			if succeeded || result.Reason != reason {
				t.Fatalf("terminal result changed: reason=%s succeeded=%t", result.Reason, succeeded)
			}
			if _, err = op.ReadProofContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("failed worker granted admission: %v", err)
			}
			if err = op.RecordWorkerSuccessContext(t.Context(), 42); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("failed observation was replaced by success: %v", err)
			}
		})
	}
}

func TestFailedWorkerObservationDoesNotRequireLocalCompletion(t *testing.T) {
	op := evidenceOperation(t)
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.RecordReceipt(42))
	mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, false))
	mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, false))

	result, succeeded, err := op.ReadWorkerResultContext(t.Context())
	mustInspection(t, err)
	if succeeded || result != nil {
		t.Fatalf("failed worker result = %#v, succeeded=%t; want exact failure without fabricated completion", result, succeeded)
	}
	if _, err = op.ReadProofContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("failed worker without completion granted admission: %v", err)
	}
	if err = op.RecordWorkerExitContext(t.Context(), 42, true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful worker without completion error = %v, want missing completion", err)
	}
}

func TestSuccessfulWorkerObservationRequiresLocalCompletion(t *testing.T) {
	op := evidenceOperation(t)
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.RecordReceipt(42))

	if err = op.RecordWorkerExitContext(t.Context(), 42, true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful worker without completion error = %v, want missing completion", err)
	}
	var observation WorkerObservationRecord
	if err = op.control.Read(workerObservationRecordName, &observation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful worker without completion published observation: %v", err)
	}
}

func TestMalformedWorkerObservationsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation WorkerObservationRecord
	}{
		{name: "success without completion digest", observation: WorkerObservationRecord{SchemaVersion: task.SchemaVersion, State: "succeeded", NumericTaskID: 42}},
		{name: "failed with invalid completion digest", observation: WorkerObservationRecord{SchemaVersion: task.SchemaVersion, State: "failed", NumericTaskID: 42, CompletionSHA256: "invalid"}},
		{name: "unknown state", observation: WorkerObservationRecord{SchemaVersion: task.SchemaVersion, State: "unknown", NumericTaskID: 42}},
		{name: "wrong receipt", observation: WorkerObservationRecord{SchemaVersion: task.SchemaVersion, State: "failed", NumericTaskID: 43}},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := evidenceOperation(t)
			permit, err := op.ClaimStart(time.Unix(101, 0))
			mustInspection(t, err)
			mustInspection(t, permit.Consume())
			mustInspection(t, op.RecordReceipt(42))
			test.observation.RequestSHA256 = op.Digest()
			if _, err = op.control.Put(workerObservationRecordName, test.observation); err != nil {
				t.Fatal(err)
			}
			if _, _, err = op.ReadWorkerResultContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("malformed observation error = %v, want evidence fault", err)
			}
		})
	}

	t.Run("wrong request", func(t *testing.T) {
		op := evidenceOperation(t)
		permit, err := op.ClaimStart(time.Unix(101, 0))
		mustInspection(t, err)
		mustInspection(t, permit.Consume())
		mustInspection(t, op.RecordReceipt(42))
		_, err = op.control.Put(workerObservationRecordName, WorkerObservationRecord{
			SchemaVersion: task.SchemaVersion,
			RequestSHA256: strings.Repeat("a", 64),
			NumericTaskID: 42,
			State:         "failed",
		})
		mustInspection(t, err)
		if _, _, err = op.ReadWorkerResultContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("wrong request observation error = %v, want evidence fault", err)
		}
	})
}

func TestWorkerObservationRequiresReceiptIdentity(t *testing.T) {
	op := evidenceOperation(t)
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())

	if err = op.RecordWorkerExitContext(t.Context(), 42, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed worker without receipt error = %v, want missing receipt", err)
	}
	var observation WorkerObservationRecord
	if err = op.control.Read(workerObservationRecordName, &observation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing receipt published worker observation: %v", err)
	}
}

func TestManagedWorkerProofRequiresWaitDependencies(t *testing.T) {
	for _, name := range []string{inspectionLifetimeTerminationName, resultRecordName, completionRecordName, startRecordName} {
		t.Run(name, func(t *testing.T) {
			store, op := completedManagedProofOperation(t)
			mustInspection(t, os.Remove(filepath.Join(store.Root, "inspections", op.Request().TaskID, name)))
			wait, err := op.ReadLifetimeWorkerWaitContext(t.Context())
			mustInspection(t, err)
			if !task.LifetimeWorkerWaitIsQualified(wait) || wait.Result != task.LifetimeWorkerWaitSuccess {
				t.Fatalf("lost qualified successful Wait: %+v", wait)
			}
			result, succeeded, err := op.ReadWorkerResultContext(t.Context())
			if result != nil || succeeded || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, task.ErrIdentityMismatch) || task.RecordAbsent(err) {
				t.Fatalf("missing %s after successful Wait: result=%+v succeeded=%t error=%v", name, result, succeeded, err)
			}
		})
	}
}

func TestManagedWorkerProofRequiresWorkerAfterWaitRead(t *testing.T) {
	store, op := completedManagedProofOperation(t)
	wait, err := op.ReadLifetimeWorkerWaitContext(t.Context())
	mustInspection(t, err)
	termination, err := op.ReadLifetimeTerminationContext(t.Context())
	mustInspection(t, err)
	// Remove the worker between the canonical Wait read and the production
	// proof reader's secondary read, without relying on scheduler timing.
	mustInspection(t, os.Remove(filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerName)))
	result, succeeded, err := op.readStartedManagedWorkerResultContext(t.Context(), wait, termination.Cause)
	if result != nil || succeeded || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, task.ErrIdentityMismatch) || task.RecordAbsent(err) {
		t.Fatalf("missing bound worker after Wait read: result=%+v succeeded=%t error=%v", result, succeeded, err)
	}
}

func TestManagedWorkerProofMissingInitialWaitRemainsAbsent(t *testing.T) {
	store, op := completedManagedProofOperation(t)
	mustInspection(t, os.Remove(filepath.Join(store.Root, "inspections", op.Request().TaskID, inspectionLifetimeWorkerWaitName)))
	result, succeeded, err := op.ReadWorkerResultContext(t.Context())
	if result != nil || succeeded || !task.RecordAbsent(err) {
		t.Fatalf("missing initial Wait is not pending: result=%+v succeeded=%t error=%v", result, succeeded, err)
	}
}

func TestObservedWorkerProofRequiresBoundCompletion(t *testing.T) {
	for _, succeeded := range []bool{true, false} {
		state := "failed"
		if succeeded {
			state = "succeeded"
		}
		for _, name := range []string{resultRecordName, completionRecordName, startRecordName} {
			t.Run(state+"/"+name, func(t *testing.T) {
				store, request, binding := inspectionFixture(t)
				op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
				mustInspection(t, err)
				t.Cleanup(func() { mustInspection(t, op.Close()) })
				permit, err := op.ClaimStart(time.Unix(101, 0))
				mustInspection(t, err)
				mustInspection(t, permit.Consume())
				mustInspection(t, op.RecordReceipt(42))
				mustInspection(t, op.Complete(ResultEligible, []byte(`{"eligible":true}`), time.Unix(102, 0)))
				mustInspection(t, op.RecordWorkerExitContext(t.Context(), 42, succeeded))
				result, observedSuccess, err := op.ReadWorkerResultContext(t.Context())
				mustInspection(t, err)
				if result == nil || observedSuccess != succeeded {
					t.Fatalf("invalid completed fixture: result=%+v succeeded=%t", result, observedSuccess)
				}
				mustInspection(t, os.Remove(filepath.Join(store.Root, "inspections", op.Request().TaskID, name)))
				result, observedSuccess, err = op.ReadWorkerResultContext(t.Context())
				if result != nil || observedSuccess || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, task.ErrIdentityMismatch) || task.RecordAbsent(err) {
					t.Fatalf("missing observed %s: result=%+v succeeded=%t error=%v", name, result, observedSuccess, err)
				}
			})
		}
	}
}

func TestManagedWorkerProofPreservesCanceledDependencyContext(t *testing.T) {
	_, op := completedManagedProofOperation(t)
	wait, err := op.ReadLifetimeWorkerWaitContext(t.Context())
	mustInspection(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, succeeded, err := op.readStartedManagedWorkerResultContext(ctx, wait, task.LifetimeTerminationCompleted)
	if result != nil || succeeded || !errors.Is(err, context.Canceled) || errors.Is(err, task.ErrIdentityMismatch) || errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("canceled dependency classified as evidence fault: result=%+v succeeded=%t error=%v", result, succeeded, err)
	}
}

func completedManagedProofOperation(t *testing.T) (*taskdir.Store, *Operation) {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	binding.WorkerExitProof = WorkerExitProofGuardianWaitV1
	now := time.Now().UTC()
	op, err := OpenOperation(store, request, binding, now)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	owner, err := op.AcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, owner.Release()) })
	registration := inspectionLifetimeRegistrationFacts(op)
	recordInspectionLifetimeForTest(t, op, registration)
	worker, err := op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
		PID: 1001, ProcessStart: "inspection-proof-start", BootID: registration.BootID,
	})
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	permit, err := op.ClaimStart(now)
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.Complete(ResultEligible, []byte(`{"eligible":true}`), now))
	_, err = op.RecordLifetimeTermination(task.LifetimeTerminationCompleted)
	mustInspection(t, err)
	_, err = op.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: digestInspectionLifetimeWorkerRecord(t, worker),
		ChildPID:     worker.PID,
		Result:       task.LifetimeWorkerWaitSuccess,
		ExitCode:     inspectionIntPointer(0),
		ObservedAt:   time.Now().UTC(),
	})
	mustInspection(t, err)
	result, succeeded, err := op.ReadWorkerResultContext(t.Context())
	mustInspection(t, err)
	if result == nil || !succeeded || result.Reason != ResultEligible {
		t.Fatalf("invalid managed proof fixture: result=%+v succeeded=%t", result, succeeded)
	}
	return store, op
}
