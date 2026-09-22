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
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 6 {
		t.Fatalf("registration=%+v", registration)
	}
	if !registration.Interpreters[0].Reference().Equal(ReferenceForMode(ModeReadOnly)) {
		t.Fatalf("interpreter reference does not bind read-only mode")
	}
	if !registration.Interpreters[1].Reference().Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatalf("interpreter reference does not bind workspace-write mode")
	}
	if !registration.Interpreters[2].Reference().Equal(readOnlyV3Reference()) {
		t.Fatalf("read-only v3 interpreter reference is not registered")
	}
	if !registration.Interpreters[3].Reference().Equal(workspaceWriteV3Reference()) {
		t.Fatalf("workspace-write v3 interpreter reference is not registered")
	}
	if !registration.Interpreters[4].Reference().Equal(readOnlyV2Reference()) {
		t.Fatalf("read-only v2 interpreter reference is not registered")
	}
	if !registration.Interpreters[5].Reference().Equal(LegacyReference()) {
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
	if got := readOnlyV3Reference().SHA256; got != "d99fb715e2c90b413712e860e2a4666bd6687f22a687823ebb236950bbc6b639" {
		t.Fatalf("read-only v3 digest changed: %s", got)
	}
	if got := workspaceWriteV3Reference().SHA256; got != "ae54dc7d7985a9a1f4f5f1bf644a6369d88ea4cfda69f7ab310490214fff1a1f" {
		t.Fatalf("workspace-write v3 digest changed: %s", got)
	}
	if got := ContractDigest(ModeReadOnly); got != "dcb001a0874aadb1dddc034f778b62fbcc5146016f3ecd2ff0c9dca0fe6f11bc" {
		t.Fatalf("read-only v4 digest changed: %s", got)
	}
	if got := ContractDigest(ModeWorkspaceWrite); got != "48f77734d7dcfb2268e4b3b835d9952779026e57bb2fa2d770539fdf4004535d" {
		t.Fatalf("workspace-write v4 digest changed: %s", got)
	}
	if strings.Contains(Contract(ModeReadOnly), "Pi workspace-write is not advertised") {
		t.Fatal("current read-only contract retains the historical workspace-write limitation")
	}
}
