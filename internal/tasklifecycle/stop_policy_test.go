package tasklifecycle

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestStopResponseFromResultDoesNotInferNativeTermination(t *testing.T) {
	id := int64(7)
	cases := []struct {
		name      string
		state     pueue.State
		numericID *int64
	}{
		{name: "ended with target", state: pueue.StateEnded, numericID: &id},
		{name: "ended without target", state: pueue.StateEnded},
		{name: "unknown with target", state: pueue.StateUnknown, numericID: &id},
		{name: "running with target", state: pueue.StateRunning, numericID: &id},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := stopOutcomeFromResult("request", "budget", pueue.StopResult{ObservedState: tc.state, NumericTaskID: tc.numericID})
			if response.terminated {
				t.Fatalf("queue state inferred native termination: %+v", response)
			}
		})
	}
}
