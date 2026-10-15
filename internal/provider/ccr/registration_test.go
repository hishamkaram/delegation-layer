package ccr

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
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
	wanted := []task.PredicateRef{
		Reference(config.ModeReadOnly), Reference(config.ModeWorkspaceWrite),
		modeledReference(config.ModeReadOnly), modeledReference(config.ModeWorkspaceWrite),
		preCredentialReference(config.ModeReadOnly, true), preCredentialReference(config.ModeWorkspaceWrite, true),
		preCredentialReference(config.ModeReadOnly, false), preCredentialReference(config.ModeWorkspaceWrite, false),
		previousReference(config.ModeReadOnly), previousReference(config.ModeWorkspaceWrite),
		HistoricalReference(config.ModeReadOnly), HistoricalReference(config.ModeWorkspaceWrite),
	}
	if len(registration.Interpreters) != len(wanted) {
		t.Fatalf("interpreters=%d want=%d", len(registration.Interpreters), len(wanted))
	}
	for index, ref := range wanted {
		if !registration.Interpreters[index].Reference().Equal(ref) {
			t.Fatalf("interpreter %d does not preserve reference %+v", index, ref)
		}
	}
}

func requireRecoveryChain(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		target task.PredicateRef
		source task.PredicateRef
	}{
		{Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly)},
		{modeledReference(config.ModeReadOnly), preCredentialReference(config.ModeReadOnly, true)},
		{preCredentialReference(config.ModeReadOnly, true), preCredentialReference(config.ModeReadOnly, false)},
		{preCredentialReference(config.ModeReadOnly, false), previousReference(config.ModeReadOnly)},
		{previousReference(config.ModeReadOnly), HistoricalReference(config.ModeReadOnly)},
	} {
		interpreter, resolveErr := registry.ResolveRecovery(pair.source)
		if resolveErr != nil || !interpreter.Target().Equal(pair.target) {
			t.Fatalf("recovery from %+v did not select %+v: %v", pair.source, pair.target, resolveErr)
		}
		if interpreter.CanRecover(pair.target) {
			t.Fatalf("recovery target accepts itself as a source: %+v", pair.target)
		}
	}
}
