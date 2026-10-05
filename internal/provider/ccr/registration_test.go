package ccr

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRegistrationKeepsCurrentAndHistoricalForegroundRefs(t *testing.T) {
	registration := Registration()
	if len(registration.Interpreters) != 4 {
		t.Fatalf("interpreters=%d want=4", len(registration.Interpreters))
	}
	if !registration.Interpreters[0].Reference().Equal(Reference(config.ModeReadOnly)) || !registration.Interpreters[1].Reference().Equal(Reference(config.ModeWorkspaceWrite)) {
		t.Fatalf("current CCR refs are not first: %+v", registration.Interpreters)
	}
	if !registration.Interpreters[2].Reference().Equal(HistoricalReference(config.ModeReadOnly)) || !registration.Interpreters[3].Reference().Equal(HistoricalReference(config.ModeWorkspaceWrite)) {
		t.Fatalf("historical CCR refs are not retained: %+v", registration.Interpreters)
	}
	recoverable, ok := registration.Interpreters[0].(interface {
		Target() task.PredicateRef
		CanRecover(task.PredicateRef) bool
	})
	if !ok || !recoverable.Target().Equal(Reference(config.ModeReadOnly)) || !recoverable.CanRecover(HistoricalReference(config.ModeReadOnly)) || recoverable.CanRecover(Reference(config.ModeReadOnly)) {
		t.Fatal("CCR current interpreter recovery mapping is incorrect")
	}
}
