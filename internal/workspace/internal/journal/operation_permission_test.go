package journal

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOperationReceiptReplayDoesNotGrantCurrentLaunchPermission(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('1'), time.Now().Add(-time.Second)))
	if err := operation.CheckLaunchPermission(t.Context()); err != nil {
		t.Fatalf("open permission = %v", err)
	}
	if err := operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatalf("historical receipt replay = %v", err)
	}
	if err := operation.CheckLaunchPermission(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("fenced permission = %v, want no-launch fence", err)
	}
	if err := operation.RecordReceipt(t.Context(), 8); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting historical receipt = %v", err)
	}
	if receipt, err := operation.Receipt(t.Context()); err != nil || receipt.NumericTaskID != 7 {
		t.Fatalf("receipt changed after refusal: %+v, %v", receipt, err)
	}
}
