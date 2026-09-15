package inspection

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
