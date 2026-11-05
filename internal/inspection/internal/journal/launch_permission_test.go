package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionLaunchPermissionSeparatesReceiptReplayFromExecution(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	binding.WorkerExitProof = WorkerExitProofGuardianWaitV1
	operation, err := OpenOperationContext(t.Context(), store, request, binding, time.Now().UTC().Add(-2*AdmissionTimeout))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	mustInspection(t, operation.CheckLaunchPermission(t.Context()))
	mustInspection(t, operation.RecordReceiptContext(t.Context(), 7))
	_, err = operation.SealLifetimeUnlaunchedContext(t.Context())
	mustInspection(t, err)
	// An exact immutable receipt remains readable after closure. It grants no
	// permission to enqueue the recorded row again.
	mustInspection(t, operation.RecordReceiptContext(t.Context(), 7))
	if err = operation.CheckLaunchPermission(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("fenced receipt replay granted execution permission: %v", err)
	}
	receipt, err := operation.ReceiptContext(t.Context())
	mustInspection(t, err)
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("permission refusal changed historical receipt: %+v", receipt)
	}
}

func TestInspectionLaunchPermissionPreservesCancellation(t *testing.T) {
	operation := evidenceOperation(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := operation.CheckLaunchPermission(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled execution permission check = %v", err)
	}
	var absent *Operation
	if err := absent.CheckLaunchPermission(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing operation granted execution permission: %v", err)
	}
}
