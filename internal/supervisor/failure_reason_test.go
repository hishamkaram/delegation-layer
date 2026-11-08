package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestFailureReasonEmptyRecoveryReasonKeepsUnderlyingClassification(t *testing.T) {
	for _, c := range []struct {
		cause error
		want  string
	}{
		{pueue.ErrPrivateSupervisorMigrationBusy, "active_work"},
		{context.DeadlineExceeded, "observation_failed"},
	} {
		wrapped := &pueue.PrivateRecoveryError{ReasonCode: "", Err: c.cause}
		if got := FailureReason(wrapped); got != c.want {
			t.Fatalf("empty recovery reason: got %q want %q", got, c.want)
		}
		if !errors.Is(wrapped, c.cause) {
			t.Fatal("reason selection changed error identity")
		}
	}
}
