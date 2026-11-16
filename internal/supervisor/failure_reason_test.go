package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestFailureReasonEmptyRecoveryReasonKeepsObservationClassification(t *testing.T) {
	// A valid empty resident reason is authoritative only for queue activity;
	// it cannot hide a client-local observation fault. Local domain builders
	// supply named reasons; bare legacy busy errors still infer queue activity.
	for _, c := range []struct {
		cause error
		want  string
	}{
		{pueue.ErrPrivateSupervisorMigrationBusy, ""},
		{context.DeadlineExceeded, "observation_failed"},
		{context.Canceled, "observation_failed"},
	} {
		wrapped := &pueue.PrivateRecoveryError{ReasonCode: "", Err: c.cause}
		if got := FailureReason(wrapped); got != c.want {
			t.Fatalf("empty recovery reason: got %q want %q", got, c.want)
		}
		if !errors.Is(wrapped, c.cause) {
			t.Fatal("reason selection changed error identity")
		}
	}
	if got := FailureReason(pueue.ErrPrivateSupervisorMigrationBusy); got != "active_work" {
		t.Fatalf("bare legacy busy reason=%q want active_work", got)
	}
}

func TestFailureReasonInvalidRecoveryReasonCannotSupplyQueueAuthority(t *testing.T) {
	wrapped := &pueue.PrivateRecoveryError{ReasonCode: "invalid-preparation-reason", Err: pueue.ErrPrivateSupervisorMigrationBusy}
	if got := FailureReason(wrapped); got != "active_work" || !errors.Is(wrapped, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("invalid reason changed queue classification or cause identity: reason=%q", got)
	}
}
