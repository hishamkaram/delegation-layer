package admission

import (
	"errors"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestManagedInspectionPreservesExactWorkerFailureWithoutCompletion(t *testing.T) {
	created := time.Now().UTC()
	_, operation := admissionfixture.NewInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	admissionfixture.RecordManagedInspectionLifetimeExit(t, operation, task.LifetimeWorkerWaitExitError, created.Add(2*time.Second))
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || succeeded || result != nil {
		t.Fatalf("worker proof = result %#v, succeeded %t, err %v; want exact failure", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if err != nil || evaluation.decision != inspectionAdmissionUnavailable || !evaluation.recorded {
		t.Fatalf("admission evaluation = %#v, err %v; want durable unavailable", evaluation, err)
	}
}

func TestManagedInspectionPreservesNotStartedFailureWithoutWorker(t *testing.T) {
	created := time.Now().UTC()
	_, operation := admissionfixture.NewInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	admissionfixture.RecordManagedInspectionNotStarted(t, operation, task.LifetimeTerminationRecovery, created.Add(2*time.Second))
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || succeeded || result != nil {
		t.Fatalf("not-started proof = result %#v, succeeded %t, err %v; want durable failure", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if err != nil || evaluation.decision != inspectionAdmissionUnavailable || !evaluation.recorded {
		t.Fatalf("not-started evaluation = %#v, err %v; want durable unavailable", evaluation, err)
	}
}

func TestManagedInspectionPreservesExactUnqualifiedWorkerFailure(t *testing.T) {
	created := time.Now().UTC()
	_, operation := admissionfixture.NewInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	admissionfixture.RecordManagedExactInspectionUnqualifiedExit(
		t, operation, task.LifetimeWorkerWaitExitError, task.LifetimeTerminationRecovery, created.Add(time.Second),
	)
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || succeeded || result != nil {
		t.Fatalf("unqualified failed worker proof = result %#v, succeeded %t, err %v; want durable failure", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if err != nil || evaluation.decision != inspectionAdmissionUnavailable || !evaluation.recorded {
		t.Fatalf("unqualified failed worker evaluation = %#v, err %v; want durable unavailable", evaluation, err)
	}
}

func TestManagedInspectionBudgetTerminationIsAdmissionExpiry(t *testing.T) {
	created := time.Now().UTC()
	_, operation := admissionfixture.NewInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipCustom, inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultExpired, nil, created.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	admissionfixture.RecordManagedInspectionLifetimeExitWithCause(
		t, operation, task.LifetimeWorkerWaitExitError, created.Add(3*time.Second), task.LifetimeTerminationBudget,
	)
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if result != nil || succeeded || !errors.Is(err, inspection.ErrAdmissionExpired) {
		t.Fatalf("budget worker proof = result %#v, succeeded %t, err %v; want admission expiry", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if evaluation.decision != inspectionAdmissionPending || evaluation.recorded || !errors.Is(err, inspection.ErrAdmissionExpired) {
		t.Fatalf("budget admission evaluation = %#v, err %v; want admission expiry", evaluation, err)
	}
}

func TestAwaitInspectionUsesDurableManagedFailureBeforeQueue(t *testing.T) {
	for _, test := range []struct {
		name    string
		created time.Time
	}{
		{name: "active admission", created: time.Now().UTC()},
		{name: "expired admission", created: time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, operation := admissionfixture.NewInspectionWorkerOperationWithExitProof(t, test.created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
			start, err := operation.ClaimStart(test.created.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err = start.Consume(); err != nil {
				t.Fatal(err)
			}
			if err = operation.RecordReceipt(42); err != nil {
				t.Fatal(err)
			}
			admissionfixture.RecordManagedInspectionLifetimeExit(t, operation, task.LifetimeWorkerWaitExitError, test.created.Add(2*time.Second))
			facts, err := AwaitInspection(t.Context(), operation, nil)
			if facts != nil || !errors.Is(err, ErrInspectionWorkerUnavailable) {
				t.Fatalf("saved failed worker required a queue read: facts=%s err=%v", facts, err)
			}
		})
	}
}
