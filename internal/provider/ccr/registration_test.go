package ccr

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRegistrationKeepsCurrentAndHistoricalForegroundRefs(t *testing.T) {
	registration := Registration()
	requireContinuationRegistration(t, registration)
	requireRegisteredReferences(t, registration)
	requireRecoveryChain(t, registration)
}

func TestPreviousReferencesRetainPreChangeContractDigests(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{mode: config.ModeReadOnly, want: "7da4847f145979d8be524c48f09e2fe3a47379996b547c5a1235c81d07f26761"},
		{mode: config.ModeWorkspaceWrite, want: "3d1b522d19843b2cced090deed9ff95630d7a45a5a26a6fb98c04511b07d94c1"},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			got := previousReference(test.mode)
			if got.SHA256 != test.want {
				t.Fatalf("previous CCR digest=%s want=%s", got.SHA256, test.want)
			}
		})
	}
}

func requireContinuationRegistration(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	if registration.ValidateContinuation == nil {
		t.Fatal("CCR registration has no exact-session continuation validator")
	}
	if slices.Contains(registration.Description.Runtime.RequiredFlags, "--resume-route") {
		t.Fatalf("runtime requirements=%q must not require optional --resume-route", registration.Description.Runtime.RequiredFlags)
	}
}

func requireRegisteredReferences(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	if len(registration.Interpreters) != 8 {
		t.Fatalf("interpreters=%d want=8", len(registration.Interpreters))
	}
	if !registration.Interpreters[0].Reference().Equal(Reference(config.ModeReadOnly)) || !registration.Interpreters[1].Reference().Equal(Reference(config.ModeWorkspaceWrite)) {
		t.Fatalf("current CCR refs are not first: %+v", registration.Interpreters)
	}
	if !registration.Interpreters[2].Reference().Equal(modeledReference(config.ModeReadOnly)) || !registration.Interpreters[3].Reference().Equal(modeledReference(config.ModeWorkspaceWrite)) || !registration.Interpreters[4].Reference().Equal(previousReference(config.ModeReadOnly)) || !registration.Interpreters[5].Reference().Equal(previousReference(config.ModeWorkspaceWrite)) || !registration.Interpreters[6].Reference().Equal(HistoricalReference(config.ModeReadOnly)) || !registration.Interpreters[7].Reference().Equal(HistoricalReference(config.ModeWorkspaceWrite)) {
		t.Fatalf("historical CCR refs are not retained: %+v", registration.Interpreters)
	}
}

func requireRecoveryChain(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	recoverable, ok := registration.Interpreters[0].(interface {
		Target() task.PredicateRef
		CanRecover(task.PredicateRef) bool
	})
	if !ok || !recoverable.Target().Equal(Reference(config.ModeReadOnly)) || !recoverable.CanRecover(modeledReference(config.ModeReadOnly)) || recoverable.CanRecover(previousReference(config.ModeReadOnly)) || recoverable.CanRecover(Reference(config.ModeReadOnly)) {
		t.Fatal("CCR current interpreter recovery mapping is incorrect")
	}
	modeled, ok := registration.Interpreters[2].(interface {
		Target() task.PredicateRef
		CanRecover(task.PredicateRef) bool
	})
	if !ok || !modeled.Target().Equal(modeledReference(config.ModeReadOnly)) || !modeled.CanRecover(previousReference(config.ModeReadOnly)) || modeled.CanRecover(HistoricalReference(config.ModeReadOnly)) {
		t.Fatal("CCR modeled interpreter recovery mapping is incorrect")
	}
	previous, ok := registration.Interpreters[4].(interface {
		Target() task.PredicateRef
		CanRecover(task.PredicateRef) bool
	})
	if !ok || !previous.Target().Equal(previousReference(config.ModeReadOnly)) || !previous.CanRecover(HistoricalReference(config.ModeReadOnly)) || previous.CanRecover(Reference(config.ModeReadOnly)) {
		t.Fatal("CCR previous interpreter recovery mapping is incorrect")
	}
}
