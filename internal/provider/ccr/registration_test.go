package ccr

import (
	"errors"
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

// TestPreAttributionReferencesRetainSealedContractDigests pins the CCR v3/v4
// digests recorded before Claude completion attribution was introduced.
func TestPreAttributionReferencesRetainSealedContractDigests(t *testing.T) {
	tests := []struct {
		name string
		got  task.PredicateRef
		want string
	}{
		{"v4/read-only", preAttributionReference(config.ModeReadOnly, true), "5c13f0e77406ce86e146532d163c5f6da74264743b1bdb6ed1ffc0f70a79f502"},
		{"v4-modeled/read-only", preAttributionReference(config.ModeReadOnly, false), "6051bed3c949dbf49eab6ced92226a5922f662b2c1c7053b72fbec3982fbdeb3"},
		{"v3/read-only", preCredentialReference(config.ModeReadOnly, true), "6dc4f085dd637a370d4a149940f8d90bb1b034de523c01ea4f22a3e0898ae641"},
		{"v3-modeled/read-only", preCredentialReference(config.ModeReadOnly, false), "7dd67b21d534d8aae367de6623924c686bd32c07d189d5cec2a4cae0a5831e43"},
		{"v4/workspace-write", preAttributionReference(config.ModeWorkspaceWrite, true), "4816a7344a169112b9b969b1339f9b418498cbc6955816d83a046fed419d89c0"},
		{"v4-modeled/workspace-write", preAttributionReference(config.ModeWorkspaceWrite, false), "e6d76ecf0ae8bf014713301c1ddc395481158cb7edb71d7a53e207cac79964e3"},
		{"v3/workspace-write", preCredentialReference(config.ModeWorkspaceWrite, true), "570f437fbc325941c7ea62d989d74f1c059b05206923978b7d5fb4ed0378e704"},
		{"v3-modeled/workspace-write", preCredentialReference(config.ModeWorkspaceWrite, false), "0c8a19bde7e0aa8a0bc14bd3c81f5bb05f5ce7fcf6d08f1449c7cf434471a824"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got.SHA256 != test.want {
				t.Fatalf("pre-attribution CCR digest=%s want=%s", test.got.SHA256, test.want)
			}
		})
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		if Reference(mode).Version != "ccr-foreground-v5" || modeledReference(mode).Version != "ccr-foreground-v5-modeled" ||
			Reference(mode).Equal(preAttributionReference(mode, true)) || modeledReference(mode).Equal(preAttributionReference(mode, false)) {
			t.Fatalf("attributed CCR references were not versioned: %+v %+v", Reference(mode), modeledReference(mode))
		}
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
		preAttributionReference(config.ModeReadOnly, true), preAttributionReference(config.ModeWorkspaceWrite, true),
		preAttributionReference(config.ModeReadOnly, false), preAttributionReference(config.ModeWorkspaceWrite, false),
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

// requireRecoveryChain asserts the direct recovery matrix: every sealed CCR
// source resolves to exactly one attributed current target, current targets do
// not recover each other, and no frozen reference remains a recovery target.
func requireRecoveryChain(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, pair := range ccrRecoveryMatrix(mode) {
			interpreter, resolveErr := registry.ResolveRecovery(pair.source)
			if resolveErr != nil || !interpreter.Target().Equal(pair.target) {
				t.Fatalf("recovery from %+v did not select %+v: %v", pair.source, pair.target, resolveErr)
			}
		}
		for _, current := range []task.PredicateRef{Reference(mode), modeledReference(mode)} {
			if _, resolveErr := registry.ResolveRecovery(current); !errors.Is(resolveErr, predicate.ErrRecoveryUnsupported) {
				t.Fatalf("current reference %s became a recovery source: %v", current.Version, resolveErr)
			}
		}
	}
	requireFrozenReferencesClaimNoRecovery(t, registration)
}

func requireFrozenReferencesClaimNoRecovery(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	for _, interpreter := range registration.Interpreters {
		recovery, ok := interpreter.(predicate.RecoveryInterpreter)
		version := interpreter.Reference().Version
		if !ok || version == currentPredicateVersion || version == modeledPredicateVersion {
			continue
		}
		for _, pair := range ccrRecoveryMatrix(interpreter.Reference().Mode) {
			if recovery.CanRecover(pair.source) {
				t.Fatalf("frozen reference %s still claims recovery of %s", version, pair.source.Version)
			}
		}
	}
}

func ccrRecoveryMatrix(mode string) []struct{ source, target task.PredicateRef } {
	return []struct{ source, target task.PredicateRef }{
		{preAttributionReference(mode, true), Reference(mode)},
		{preCredentialReference(mode, true), Reference(mode)},
		{preAttributionReference(mode, false), modeledReference(mode)},
		{preCredentialReference(mode, false), modeledReference(mode)},
		{previousReference(mode), modeledReference(mode)},
		{HistoricalReference(mode), modeledReference(mode)},
	}
}
