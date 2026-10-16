package workspace

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAbortedWorkspaceAdmissionCannotReviveAfterAnotherUse(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	subject := Subject{Kind: SubjectAdmission, ID: testID('a'), SHA256: testSHA('b')}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordAdmissionAborted(t.Context(), reservation); err != nil {
		t.Fatal(err)
	}
	next, err := resource.Reserve(t.Context(), Subject{Kind: SubjectOperation, ID: testID('c'), SHA256: testSHA('d')})
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.CloseReservation(t.Context(), next, "operation-completed", ""); err != nil {
		t.Fatal(err)
	}
	retried, err := resource.Reserve(t.Context(), subject)
	if err != nil || retried.Sequence != next.Sequence+1 {
		t.Fatalf("explicit retry did not get a new use reservation: %+v %v", retried, err)
	}
	closed, err := resource.ReservationClosed(t.Context(), reservation.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("new attempt revived the original closed reservation: %+v %v", closed, err)
	}
}

func TestUnlaunchedCaptureReceiptHasDistinctProofVariant(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	receipt := CaptureReceipt{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: testID('e'), Subject: Subject{Kind: SubjectTask, ID: testID('f'), SHA256: testSHA('a')}, Status: "unavailable", ReasonCode: "capture_not_launched", UnlaunchedSHA256: testSHA('b')}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	stored, err := resource.CaptureReceipt(t.Context(), receipt.OperationID)
	if err != nil || stored.UnlaunchedSHA256 != receipt.UnlaunchedSHA256 || stored.TerminationSHA256 != "" || stored.WorkerWaitSHA256 != "" {
		t.Fatalf("unlaunched proof was replaced by native termination: %+v, %v", stored, err)
	}
	receipt.OperationID = testID('c')
	receipt.TerminationSHA256 = testSHA('d')
	if err = resource.RecordCaptureReceipt(t.Context(), receipt); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("mixed native and unlaunched proof accepted: %v", err)
	}
}
