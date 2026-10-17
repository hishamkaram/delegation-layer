package guardian

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type closureTestRecords struct {
	evidence   task.LifetimeEvidence
	fence      *task.LifetimeUnlaunchedRecord
	fenceErr   error
	waitErr    error
	reads      []string
	fenceReads int
}

func (r *closureTestRecords) ReadLifetimeIntentContext(context.Context) (*task.LifetimeIntentRecord, error) {
	r.reads = append(r.reads, "intent")
	return r.evidence.Intent, nil
}

func (r *closureTestRecords) ReadLifetimeRegistrationContext(context.Context) (*task.LifetimeRegistrationRecord, error) {
	r.reads = append(r.reads, "registration")
	return r.evidence.Registration, nil
}

func (r *closureTestRecords) ReadLifetimeWorkerContext(context.Context) (*task.LifetimeWorkerRecord, error) {
	r.reads = append(r.reads, "worker")
	return r.evidence.Worker, nil
}

func (r *closureTestRecords) ReadLifetimeWorkerWaitContext(context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	r.reads = append(r.reads, "wait")
	return r.evidence.WorkerWait, r.waitErr
}

func (r *closureTestRecords) ReadLifetimeLaunchesContext(context.Context) ([]task.LifetimeLaunchRecord, error) {
	r.reads = append(r.reads, "launches")
	return r.evidence.Launches, nil
}

func (r *closureTestRecords) ReadLifetimeTerminationContext(context.Context) (*task.LifetimeTerminationRecord, error) {
	r.reads = append(r.reads, "termination")
	return r.evidence.Termination, nil
}

func (r *closureTestRecords) ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	r.fenceReads++
	if r.fenceErr != nil {
		return nil, r.fenceErr
	}
	if r.fence == nil {
		return nil, os.ErrNotExist
	}
	return r.fence, nil
}

type closureHashRecord interface {
	task.LifetimeIntentRecord | task.LifetimeRegistrationRecord | task.LifetimeWorkerRecord |
		task.LifetimeWorkerWaitRecord | task.LifetimeTerminationRecord | task.LifetimeUnlaunchedRecord | []task.LifetimeLaunchRecord
}

func closureTestHash[T closureHashRecord](t *testing.T, record T) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	return task.ComputeSHA256(data)
}

func newClosureTestRecords(t *testing.T, family string) *closureTestRecords {
	t.Helper()
	intent := task.LifetimeIntentRecord{
		SchemaVersion: task.SchemaVersion, RootID: lifetimeGuardianTestRootID, TaskID: lifetimeGuardianTestTaskID,
		OwnerKind: family, OwnerSHA256: task.ComputeSHA256([]byte("closure-owner")),
		ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact,
		ScopePlan: "closure-plan", BootID: "closure-boot",
		Deadline: "2026-10-16T12:00:00Z", CreatedAt: "2026-10-16T11:00:00Z",
	}
	registration := task.LifetimeRegistrationRecord{
		SchemaVersion: intent.SchemaVersion, RootID: intent.RootID, TaskID: intent.TaskID,
		OwnerKind: intent.OwnerKind, OwnerSHA256: intent.OwnerSHA256,
		ScopeKind: intent.ScopeKind, Assurance: intent.Assurance, ScopeID: "closure-scope",
		BootID: intent.BootID, Deadline: intent.Deadline, CreatedAt: "2026-10-16T11:01:00Z",
	}
	registrationHash := closureTestHash(t, registration)
	worker := task.LifetimeWorkerRecord{
		SchemaVersion: task.SchemaVersion, RootID: intent.RootID, TaskID: intent.TaskID,
		RegistrationSHA256: registrationHash, PID: 321, ProcessStart: "native:321",
		BootID: intent.BootID, StartedAt: "2026-10-16T11:02:00Z",
	}
	exitCode := 0
	wait := task.LifetimeWorkerWaitRecord{
		SchemaVersion: task.SchemaVersion, RootID: intent.RootID, TaskID: intent.TaskID,
		RegistrationSHA256: registrationHash, WorkerSHA256: closureTestHash(t, worker),
		ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &exitCode,
		ObservedAt: "2026-10-16T11:03:00Z",
	}
	termination := task.LifetimeTerminationRecord{
		SchemaVersion: task.SchemaVersion, RootID: intent.RootID, TaskID: intent.TaskID,
		RegistrationSHA256: registrationHash, LaunchSetSHA256: closureTestHash(t, []task.LifetimeLaunchRecord{}),
		Cause: task.LifetimeTerminationCompleted, ScopeEmpty: true, ObservedAt: "2026-10-16T11:04:00Z",
	}
	return &closureTestRecords{evidence: task.LifetimeEvidence{
		Intent: &intent, Registration: &registration, Worker: &worker, WorkerWait: &wait, Termination: &termination,
	}}
}

func TestClosureProofPreservesCanonicalHashesAcrossRecordFamilies(t *testing.T) {
	for _, family := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection, task.LifetimeOwnerWorkspace} {
		t.Run(family, func(t *testing.T) {
			records := newClosureTestRecords(t, family)
			proof, err := ReadAuxiliaryClosureProof(t.Context(), records)
			if err != nil || !proof.Closed() || proof.Kind() != task.LifetimeClosureRegisteredNative {
				t.Fatalf("native closure = %+v err=%v", proof, err)
			}
			if proof.TerminationSHA256() != closureTestHash(t, *records.evidence.Termination) ||
				proof.WorkerWaitSHA256() != closureTestHash(t, *records.evidence.WorkerWait) || proof.UnlaunchedSHA256() != "" {
				t.Fatalf("native closure hashes = %+v", proof)
			}
			if records.fenceReads != 0 || !slices.Equal(records.reads, []string{"wait", "termination", "launches", "worker", "registration", "intent"}) {
				t.Fatalf("record read order=%v fence reads=%d", records.reads, records.fenceReads)
			}
			saved := proof
			records.evidence.Termination.Cause = task.LifetimeTerminationRequested
			if proof != saved || proof.TerminationSHA256() == closureTestHash(t, *records.evidence.Termination) {
				t.Fatal("record mutation changed the immutable closure projection")
			}
		})
	}
}

func TestClosureProofRejectsMixedFamilyEvidenceBeforeFence(t *testing.T) {
	records := newClosureTestRecords(t, task.LifetimeOwnerWorkspace)
	records.evidence.Intent.OwnerKind = task.LifetimeOwnerInspection
	proof, err := ReadAuxiliaryClosureProof(t.Context(), records)
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, task.ErrIdentityMismatch) || proof.Closed() {
		t.Fatalf("mixed family closure = %+v err=%v", proof, err)
	}
	if records.fenceReads != 0 {
		t.Fatal("invalid native evidence fell through to an unlaunched fence")
	}
}

func TestClosureProofDoesNotInferWaitOrTermination(t *testing.T) {
	for _, missing := range []string{"wait", "termination"} {
		t.Run(missing, func(t *testing.T) {
			records := newClosureTestRecords(t, task.LifetimeOwnerInspection)
			if missing == "wait" {
				records.evidence.WorkerWait = nil
			} else {
				records.evidence.Termination = nil
			}
			proof, err := ReadAuxiliaryClosureProof(t.Context(), records)
			if !errors.Is(err, ErrProofPending) || proof.Closed() || proof != (ClosureProof{}) || records.fenceReads != 1 {
				t.Fatalf("missing %s: proof=%+v err=%v fence reads=%d", missing, proof, err, records.fenceReads)
			}
		})
	}
}

func TestClosureProofPreservesRecoveryDisposition(t *testing.T) {
	records := newClosureTestRecords(t, task.LifetimeOwnerInspection)
	records.evidence = task.LifetimeEvidence{Intent: records.evidence.Intent}
	absence, err := task.NewLifetimeWorkerAbsence(records.evidence, task.LifetimeAbsenceUnregisteredDisposed,
		records.evidence.Intent.BootID, time.Date(2026, time.October, 16, 11, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	records.evidence.WorkerWait = &absence
	proof, err := ReadClosureProof(t.Context(), records)
	if err != nil || !proof.Closed() || proof.Kind() != task.LifetimeClosureUnregisteredRecovered ||
		proof.WorkerWaitSHA256() != closureTestHash(t, absence) || proof.TerminationSHA256() != "" || proof.UnlaunchedSHA256() != "" {
		t.Fatalf("unregistered closure = %+v err=%v", proof, err)
	}
}

func TestAuxiliaryClosureProofKeepsFenceSeparateFromNativeClosure(t *testing.T) {
	for _, family := range []string{task.LifetimeOwnerInspection, task.LifetimeOwnerWorkspace} {
		t.Run(family, func(t *testing.T) {
			fence := task.LifetimeUnlaunchedRecord{
				SchemaVersion: task.SchemaVersion, RootID: lifetimeGuardianTestRootID, TaskID: lifetimeGuardianTestTaskID,
				OwnerKind: family, OwnerSHA256: task.ComputeSHA256([]byte("fenced-owner")),
				Deadline: "2026-10-16T12:00:00Z", Cause: task.LifetimeUnlaunchedCauseDeadlineExpired, ObservedAt: "2026-10-16T12:01:00Z",
			}
			if err := task.ValidateLifetimeUnlaunchedRecord(&fence); err != nil {
				t.Fatal(err)
			}
			records := &closureTestRecords{fence: &fence}
			proof, err := ReadAuxiliaryClosureProof(t.Context(), records)
			if err != nil || !proof.Closed() || proof.Kind() != "" || proof.TerminationSHA256() != "" ||
				proof.WorkerWaitSHA256() != "" || proof.UnlaunchedSHA256() != closureTestHash(t, fence) {
				t.Fatalf("fence closure = %+v err=%v", proof, err)
			}
		})
	}
}

func TestAuxiliaryClosureProofPreservesMarkedAbsence(t *testing.T) {
	for _, cause := range []error{task.ErrEvidenceFault, task.ErrIdentityMismatch, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			fault := errors.Join(os.ErrNotExist, cause)
			records := &closureTestRecords{fenceErr: fault}
			proof, err := ReadAuxiliaryClosureProof(t.Context(), records)
			if !errors.Is(err, fault) || errors.Is(err, ErrProofPending) || proof.Closed() {
				t.Fatalf("marked fence absence = %+v err=%v", proof, err)
			}
			records.waitErr = fault
			records.fenceReads = 0
			proof, err = ReadAuxiliaryClosureProof(t.Context(), records)
			if !errors.Is(err, fault) || proof.Closed() || records.fenceReads != 0 {
				t.Fatalf("marked Wait absence = %+v err=%v fence reads=%d", proof, err, records.fenceReads)
			}
		})
	}
}

func TestClosureProofHonorsCanceledReader(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	records := &closureTestRecords{}
	proof, err := ReadAuxiliaryClosureProof(ctx, records)
	if !errors.Is(err, context.Canceled) || proof.Closed() || len(records.reads) != 0 || records.fenceReads != 0 {
		t.Fatalf("canceled proof=%+v err=%v reads=%v fence reads=%d", proof, err, records.reads, records.fenceReads)
	}
}
