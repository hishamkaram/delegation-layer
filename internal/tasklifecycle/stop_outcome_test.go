package tasklifecycle

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestStopOutcomeRetainsImmutableObservedFacts(t *testing.T) {
	id, acknowledged := int64(7), true
	outcome := stopOutcomeFromResult("request", "user", pueue.StopResult{
		NumericTaskID: &id, Acknowledged: &acknowledged,
		ObservedState: pueue.StateEnded,
	})
	id, acknowledged = 8, false
	observedID, observedAcknowledgement := outcome.NumericTaskID(), outcome.Acknowledged()
	if observedID == nil || *observedID != 7 || observedAcknowledgement == nil || !*observedAcknowledgement {
		t.Fatalf("outcome followed mutable input: id %v, acknowledgement %v", observedID, observedAcknowledgement)
	}
	*observedID, *observedAcknowledgement = 9, false
	if *outcome.NumericTaskID() != 7 || !*outcome.Acknowledged() {
		t.Fatal("consumer mutation changed retained stop facts")
	}
	if outcome.Terminated() {
		t.Fatal("observed queue completion fabricated native termination")
	}
	if (StopOutcome{}).NumericTaskID() != nil || (StopOutcome{}).Acknowledged() != nil {
		t.Fatal("empty outcome fabricated optional stop facts")
	}
}
