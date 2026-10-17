package journal

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAdmissionAttemptIdentityUsesImmutableReservationSequence(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	subject := Subject{Kind: SubjectAdmission, ID: testID('a'), SHA256: testSHA('b')}
	first, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	before, err := resource.ResolveAdmissionAttempt(t.Context(), first.Sequence, subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordAdmissionAborted(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	after, err := resource.ResolveAdmissionAttempt(t.Context(), second.Sequence, subject)
	if err != nil {
		t.Fatal(err)
	}
	assertDistinctAdmissionAttemptCoordinates(t, before, after)
	replayed, err := resource.ResolveAdmissionAttempt(t.Context(), first.Sequence, subject)
	if err != nil || replayed != before {
		t.Fatalf("recovery changed original attempt identity: %+v %v", replayed, err)
	}
}

func assertDistinctAdmissionAttemptCoordinates(t *testing.T, before, after AdmissionAttempt) {
	t.Helper()
	firstID, err := before.InspectionOperationID()
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := after.InspectionOperationID()
	if err != nil {
		t.Fatal(err)
	}
	firstPurpose, err := before.ProvisionPurpose()
	if err != nil {
		t.Fatal(err)
	}
	secondPurpose, err := after.ProvisionPurpose()
	if err != nil {
		t.Fatal(err)
	}
	if before.TaskID != after.TaskID || before.RequestSHA256 != after.RequestSHA256 || firstID == secondID || firstPurpose == secondPurpose {
		t.Fatalf("retry reused helper identity or changed logical request: before=%+v after=%+v", before, after)
	}
}

func TestAdmissionAttemptRejectsUnboundSubjectAndNonAbortedRetry(t *testing.T) {
	_, resource, _ := newResourceFixture(t)
	defer closeResource(t, resource)
	subject := Subject{Kind: SubjectAdmission, ID: testID('a'), SHA256: testSHA('b')}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	other := subject
	other.SHA256 = testSHA('c')
	if _, err = resource.ResolveAdmissionAttempt(t.Context(), reservation.Sequence, other); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("attempt accepted another request digest: %v", err)
	}
	if err = resource.CloseReservation(t.Context(), reservation, "operation-completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = resource.Reserve(t.Context(), subject); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("non-aborted admission authorized another attempt: %v", err)
	}
}

func TestAdmissionInspectionIdentityHasStableDomainSeparatedEncoding(t *testing.T) {
	attempt := AdmissionAttempt{
		RootID: strings.Repeat("a", 32), WorkspaceID: strings.Repeat("b", 32),
		TaskID: strings.Repeat("c", 32), RequestSHA256: strings.Repeat("d", 64), Sequence: 1,
	}
	id, err := attempt.InspectionOperationID()
	if err != nil || id != "aa14f3ed82ed31213824332e17ab25bf" {
		t.Fatalf("canonical attempt identity changed: %q %v", id, err)
	}
}
