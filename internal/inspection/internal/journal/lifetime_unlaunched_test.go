package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionUnlaunchedFenceRefusesLateAuthority(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	binding.WorkerExitProof = WorkerExitProofGuardianWaitV1
	created := time.Now().UTC().Add(-2 * AdmissionTimeout)
	operation, err := OpenOperationContext(t.Context(), store, request, binding, created)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	fence, err := operation.SealLifetimeUnlaunchedContext(t.Context())
	mustInspection(t, err)
	if fence.OwnerKind != task.LifetimeOwnerInspection {
		t.Fatalf("wrong fence owner: %+v", fence)
	}
	for _, call := range []struct {
		name   string
		invoke func() error
	}{
		{"submission", func() error {
			_, invokeErr := operation.ClaimSubmissionContext(t.Context(), created.Add(time.Second))
			return invokeErr
		}},
		{"start", func() error {
			_, invokeErr := operation.ClaimStartContext(t.Context(), created.Add(time.Second))
			return invokeErr
		}},
		{"receipt", func() error { return operation.RecordReceiptContext(t.Context(), 7) }},
		{"native intent", func() error {
			_, invokeErr := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "late-plan", BootID: "late-boot", Deadline: operation.Deadline()})
			return invokeErr
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			if invokeErr := call.invoke(); !errors.Is(invokeErr, task.ErrLifetimeUnlaunched) {
				t.Fatalf("late authority = %v, want unlaunched refusal", invokeErr)
			}
		})
	}
	for _, name := range []string{submissionRecordName, startRecordName, receiptRecordName, inspectionLifetimeIntentName, inspectionLifetimeRegistrationName} {
		if _, statErr := os.Lstat(filepath.Join(store.Root, "inspections", request.TaskID, name)); !os.IsNotExist(statErr) {
			t.Fatalf("fenced inspection created %s: %v", name, statErr)
		}
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if result != nil || succeeded || !errors.Is(err, ErrAdmissionExpired) {
		t.Fatalf("unlaunched result = %+v, %t, %v", result, succeeded, err)
	}
}

func TestInspectionReceiptReplayAfterFenceRemainsReadbackOnly(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership, binding.WorkerExitProof = task.RunnerOwnershipManaged, WorkerExitProofGuardianWaitV1
	created := time.Now().UTC().Add(-2 * AdmissionTimeout)
	operation, err := OpenOperationContext(t.Context(), store, request, binding, created)
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	mustInspection(t, operation.RecordReceiptContext(t.Context(), 7))
	_, err = operation.SealLifetimeUnlaunchedContext(t.Context())
	mustInspection(t, err)
	mustInspection(t, operation.RecordReceiptContext(t.Context(), 7))
	if err = operation.RecordReceiptContext(t.Context(), 8); err == nil {
		t.Fatal("closed inspection replaced its numeric receipt")
	}
	if _, err = operation.ClaimStartContext(t.Context(), created.Add(time.Second)); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("receipt readback reopened start authorization: %v", err)
	}
	receipt, err := operation.ReceiptContext(t.Context())
	mustInspection(t, err)
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("receipt readback changed immutable identity: %+v", receipt)
	}
}
