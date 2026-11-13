package infrastructure_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

type retainedRuntimeWireError struct{ error }

func (e *retainedRuntimeWireError) Unwrap() error         { return e.error }
func (*retainedRuntimeWireError) RetainsResidentCleanup() {}

// The real application classifier feeds the actual server/failReply and strict
// Unix wire. No test chooses the returned condition or repeats its predicate.
func TestRuntimeRestartApplicationClassifierControlsActualWireCondition(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		cause       error
		condition   infrastructure.PreparationCondition
		bits        infrastructure.PreparationCauses
		independent bool
	}{
		{"deadline", context.DeadlineExceeded, infrastructure.PreparationPending, infrastructure.PreparationCauseDeadline, false},
		{"canceled", context.Canceled, infrastructure.PreparationPending, infrastructure.PreparationCauseCanceled, false},
		{"inflight", pueue.ErrInFlight, infrastructure.PreparationPending, infrastructure.PreparationCauseInFlight, false},
		{"mixed binding", errors.Join(context.DeadlineExceeded, pueue.ErrBinding), infrastructure.PreparationTimeout, infrastructure.PreparationCauseDeadline | infrastructure.PreparationCauseBinding, false},
		{"retained cleanup", &retainedRuntimeWireError{error: context.DeadlineExceeded}, infrastructure.PreparationFault, infrastructure.PreparationCauseDeadline, true},
		{"wrong root", infrastructure.ErrIdentity, infrastructure.PreparationFault, infrastructure.PreparationCauseInfrastructureIdentity, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cause := &pueue.PrivateRuntimeRestartError{ReasonCode: "successor_not_ready", Err: errors.Join(pueue.ErrPrivateRuntimeRestartPending, fixture.cause)}
			outcome, err := infrastructure.RuntimeRestartWireForTest(t, cause, (infrastructurehost.Host{}).ClassifyPreparation)
			if err != nil || outcome.Validate() != nil {
				t.Fatalf("actual application/server wire failed: outcome=%+v err=%v", outcome, err)
			}
			if outcome.Condition != fixture.condition || outcome.Failure.Causes != infrastructure.PreparationCauseRuntimeRestartPending|fixture.bits || outcome.Failure.IndependentFault != fixture.independent {
				t.Fatalf("actual server condition lost typed pending or hid hard fault: got=%+v want=%s", outcome, fixture.condition)
			}
		})
	}
	bare, err := infrastructure.RuntimeRestartWireForTest(t, context.DeadlineExceeded, (infrastructurehost.Host{}).ClassifyPreparation)
	if err != nil || bare.Condition != infrastructure.PreparationTimeout || bare.Failure.Causes != infrastructure.PreparationCauseDeadline {
		t.Fatalf("runtime correction changed ordinary deadline classification: %+v err=%v", bare, err)
	}
}
