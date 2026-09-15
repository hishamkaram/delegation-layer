package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func evidenceOperation(t *testing.T) *Operation {
	t.Helper()
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	return op
}

func TestInspectionCompletionRequiresStartAndMatchingResult(t *testing.T) {
	op := evidenceOperation(t)
	now := time.Unix(102, 0)
	facts := json.RawMessage(`{"eligible":true}`)
	if err := op.Complete(ResultEligible, facts, now); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completion without start: %v", err)
	}
	permit, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, permit.Consume())
	mustInspection(t, op.Complete(ResultEligible, facts, now))
	result, completion, err := op.ReadCompleted()
	mustInspection(t, err)
	if result.Reason != ResultEligible || completion.RequestSHA256 != op.Digest() {
		t.Fatal("completion identity lost")
	}
	mustInspection(t, op.Complete(ResultEligible, facts, now.Add(time.Second)))
	_, replayed, err := op.ReadCompleted()
	mustInspection(t, err)
	if replayed != completion {
		t.Fatal("replay changed completion time")
	}
	if err = op.Complete(ResultEligible, json.RawMessage(`{"eligible":false}`), now); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("conflicting result accepted")
	}
}

func TestInspectionResultWithoutCompletionIsIneligible(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.control.Put(resultRecordName, ResultRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: op.Digest(), Reason: ResultEligible, Facts: map[string]json.RawMessage{"eligible": json.RawMessage(`true`)}})
	mustInspection(t, err)
	if _, _, err = op.ReadCompleted(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload alone qualified: %v", err)
	}
}

func TestInspectionCompletionRejectsLateOrSecretDiagnosticFacts(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	for _, test := range []struct {
		reason ResultReason
		facts  json.RawMessage
		now    time.Time
	}{
		{ResultEligible, json.RawMessage(`{}`), time.Unix(120, 0)},
		{ResultEligible, json.RawMessage(`null`), time.Unix(102, 0)},
		{ResultUnavailable, json.RawMessage(`{"error":"synthetic-secret"}`), time.Unix(102, 0)},
		{ResultReason("synthetic-secret"), nil, time.Unix(102, 0)},
		{ResultEligible, json.RawMessage(`{}`), time.Unix(100, 0)},
	} {
		if err = op.Complete(test.reason, test.facts, test.now); err == nil {
			t.Fatal("invalid completion accepted")
		}
	}
	mustInspection(t, op.Complete(ResultExpired, nil, time.Unix(121, 0)))
	result, _, err := op.ReadCompleted()
	mustInspection(t, err)
	if result.Reason != ResultExpired || len(result.Facts) != 0 {
		t.Fatal("expired result contains eligible data")
	}
}

func TestInspectionReceiptDoesNotDefaultMissingTargetToZero(t *testing.T) {
	for _, target := range []map[string]any{{}, {"numeric_task_id": nil}, {"numeric_task_id": -1}} {
		op := evidenceOperation(t)
		target["schema_version"] = task.SchemaVersion
		target["request_sha256"] = op.Digest()
		_, err := op.control.Put(receiptRecordName, target)
		mustInspection(t, err)
		if _, err = op.Receipt(); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("missing target was accepted: %v", err)
		}
	}
	op := evidenceOperation(t)
	mustInspection(t, op.RecordReceipt(0))
	receipt, err := op.Receipt()
	mustInspection(t, err)
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != 0 {
		t.Fatal("valid zero target lost")
	}
	if err = op.RecordReceipt(1); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("receipt target changed")
	}
}

func TestInspectionClaimRejectsTimeBeforeCreation(t *testing.T) {
	op := evidenceOperation(t)
	if permit, err := op.ClaimStart(time.Unix(99, 0)); permit != nil || !errors.Is(err, ErrInvalidAdmissionTime) {
		t.Fatal("pre-creation start was authorized")
	}
	if permit, err := op.ClaimSubmission(time.Unix(99, 0)); permit != nil || !errors.Is(err, ErrInvalidAdmissionTime) {
		t.Fatal("pre-creation submission was authorized")
	}
}

func TestCanceledReceiptContextCannotPublishAuthority(t *testing.T) {
	op := evidenceOperation(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := op.RecordReceiptContext(ctx, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled receipt context returned %v", err)
	}
	if _, err := op.Receipt(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled receipt context published authority: %v", err)
	}
}

func TestCanceledCompletedReadHonorsCallerContext(t *testing.T) {
	op := evidenceOperation(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := op.ReadCompletedContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled completed read returned %v", err)
	}
}
