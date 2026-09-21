package pi

import (
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionExposesModesAndNativeOptions(t *testing.T) {
	description := Description()
	requirePiDescription(t, description)
	registration := Registration()
	requirePiInterpreters(t, registration)
	if got := ReferenceForMode(config.ModeWorkspaceWrite); got == (task.PredicateRef{}) || got.Equal(ReferenceForMode(ModeReadOnly)) {
		t.Fatalf("workspace-write predicate reference=%+v", got)
	}
}

func requirePiDescription(t *testing.T, description commonprovider.Description) {
	t.Helper()
	if description.ID != Provider || !description.Discoverable {
		t.Fatalf("unexpected description: %+v", description)
	}
	if !slices.Equal(description.SupportedModes, []string{ModeReadOnly, ModeWorkspaceWrite}) {
		t.Fatalf("supported modes=%q", description.SupportedModes)
	}
	for _, option := range []string{commonprovider.OptionContinuation, commonprovider.OptionModel, commonprovider.OptionEffort} {
		if !slices.Contains(description.SupportedOptions, option) {
			t.Fatalf("supported options=%q missing %q", description.SupportedOptions, option)
		}
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatal("runtime flag requirements are missing")
	}
}

func requirePiInterpreters(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 4 {
		t.Fatalf("registration=%+v", registration)
	}
	if !registration.Interpreters[0].Reference().Equal(ReferenceForMode(ModeReadOnly)) {
		t.Fatalf("interpreter reference does not bind read-only mode")
	}
	if !registration.Interpreters[1].Reference().Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatalf("interpreter reference does not bind workspace-write mode")
	}
	if !registration.Interpreters[2].Reference().Equal(readOnlyV2Reference()) {
		t.Fatalf("read-only v2 interpreter reference is not registered")
	}
	if !registration.Interpreters[3].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy interpreter reference is not registered")
	}
}

func TestReadOnlyPredicateReferencesRemainStable(t *testing.T) {
	if got := readOnlyV2Reference().SHA256; got != "ec70ff51fb9ad100dead3efa5775ac0a2d1ee20374d2872643ba468e8fd2d583" {
		t.Fatalf("read-only v2 digest changed: %s", got)
	}
	if got := LegacyReference().SHA256; got != "e314d5c39aba88f76d104ccc8a91d1265893167866eba1f206d347d67d7d0e45" {
		t.Fatalf("read-only v1 digest changed: %s", got)
	}
	if got := ContractDigest(ModeReadOnly); got != "e4199890ec9de6111af7411a025cbaf1dbbbae96d6ca66f870dc3e89f06ffc44" {
		t.Fatalf("read-only v3 digest changed: %s", got)
	}
	if strings.Contains(Contract(ModeReadOnly), "Pi workspace-write is not advertised") {
		t.Fatal("current read-only contract retains the historical workspace-write limitation")
	}
}
