package pi

import (
	"errors"
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

func TestExactContinuationRequiresSameWorkspace(t *testing.T) {
	predecessor := task.TaskRecord{CanonicalCwd: "/workspace"}
	successor := predecessor
	if err := validateContinuation(predecessor, task.MetaRecord{}, successor); err != nil {
		t.Fatalf("same-workspace continuation error = %v", err)
	}
	successor.CanonicalCwd = "/other-workspace"
	if err := validateContinuation(predecessor, task.MetaRecord{}, successor); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-workspace continuation error = %v", err)
	}
	if Registration().ValidateContinuation == nil {
		t.Fatal("Pi registration has no workspace continuation validator")
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
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 11 {
		t.Fatalf("registration=%+v", registration)
	}
	if !registration.Interpreters[0].Reference().Equal(ReferenceForMode(ModeReadOnly)) {
		t.Fatalf("interpreter reference does not bind read-only mode")
	}
	if !registration.Interpreters[1].Reference().Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatalf("interpreter reference does not bind workspace-write mode")
	}
	if !registration.Interpreters[2].Reference().Equal(workspaceWriteV7Reference()) {
		t.Fatalf("workspace-write v7 interpreter reference is not registered")
	}
	if !registration.Interpreters[3].Reference().Equal(readOnlyV5Reference()) {
		t.Fatalf("read-only v5 interpreter reference is not registered")
	}
	if !registration.Interpreters[4].Reference().Equal(workspaceWriteV5Reference()) {
		t.Fatalf("workspace-write v5 interpreter reference is not registered")
	}
	if !registration.Interpreters[5].Reference().Equal(readOnlyV4Reference()) {
		t.Fatalf("read-only v4 interpreter reference is not registered")
	}
	if !registration.Interpreters[6].Reference().Equal(workspaceWriteV4Reference()) {
		t.Fatalf("workspace-write v4 interpreter reference is not registered")
	}
	if !registration.Interpreters[7].Reference().Equal(readOnlyV3Reference()) {
		t.Fatalf("read-only v3 interpreter reference is not registered")
	}
	if !registration.Interpreters[8].Reference().Equal(workspaceWriteV3Reference()) {
		t.Fatalf("workspace-write v3 interpreter reference is not registered")
	}
	if !registration.Interpreters[9].Reference().Equal(readOnlyV2Reference()) {
		t.Fatalf("read-only v2 interpreter reference is not registered")
	}
	if !registration.Interpreters[10].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy interpreter reference is not registered")
	}
}

func TestPredicateReferencesRemainStable(t *testing.T) {
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
	if got := readOnlyV4Reference().SHA256; got != "dcb001a0874aadb1dddc034f778b62fbcc5146016f3ecd2ff0c9dca0fe6f11bc" {
		t.Fatalf("read-only v4 digest changed: %s", got)
	}
	if got := readOnlyV5Reference().SHA256; got != "cd9b1fcd05395a13ddda10de45a458bbd1d21200348508214570b6b02797330b" {
		t.Fatalf("read-only v5 digest changed: %s", got)
	}
	if got := workspaceWriteV4Reference().SHA256; got != "48f77734d7dcfb2268e4b3b835d9952779026e57bb2fa2d770539fdf4004535d" {
		t.Fatalf("workspace-write v4 digest changed: %s", got)
	}
	if got := workspaceWriteV7Reference().SHA256; got != "2136f5a6611eaa4aaec6f91237b823317b18462046ed29a76c2043abdc632e4f" {
		t.Fatalf("workspace-write v7 digest changed: %s", got)
	}
	if ReferenceForMode(ModeReadOnly).Version != currentReadOnlyVersion || ReferenceForMode(ModeReadOnly).SHA256 != ContractDigest(ModeReadOnly) {
		t.Fatalf("current read-only reference is not bound to v6 contract: %+v", ReferenceForMode(ModeReadOnly))
	}
	if ReferenceForMode(ModeWorkspaceWrite).Version != currentWorkspaceVersion || ReferenceForMode(ModeWorkspaceWrite).SHA256 != ContractDigest(ModeWorkspaceWrite) {
		t.Fatalf("current workspace-write reference is not bound to v8 contract: %+v", ReferenceForMode(ModeWorkspaceWrite))
	}
	if workspaceWriteV7Reference().Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatal("workspace-write v7 predicate was not retained as historical")
	}
	if strings.Contains(Contract(ModeReadOnly), "Pi workspace-write is not advertised") {
		t.Fatal("current read-only contract retains the historical workspace-write limitation")
	}
}
