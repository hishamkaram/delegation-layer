package pi

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
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
	expected := []struct {
		name      string
		reference task.PredicateRef
	}{
		{"current read-only", ReferenceForMode(ModeReadOnly)},
		{"current workspace-write", ReferenceForMode(ModeWorkspaceWrite)},
		{"read-only v6", readOnlyV6Reference()},
		{"workspace-write v8", workspaceWriteV8Reference()},
		{"workspace-write v7", workspaceWriteV7Reference()},
		{"read-only v5", readOnlyV5Reference()},
		{"workspace-write v5", workspaceWriteV5Reference()},
		{"read-only v4", readOnlyV4Reference()},
		{"workspace-write v4", workspaceWriteV4Reference()},
		{"read-only v3", readOnlyV3Reference()},
		{"workspace-write v3", workspaceWriteV3Reference()},
		{"read-only v2", readOnlyV2Reference()},
		{"legacy", LegacyReference()},
	}
	if registration.Prepare == nil || registration.PrepareExisting == nil || registration.PrepareSuccessor == nil {
		t.Fatalf("registration=%+v", registration)
	}
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatalf("invalid or duplicate registered reference: %v", err)
	}
	for _, want := range expected {
		if _, err := registry.Resolve(want.reference); err != nil {
			t.Fatalf("%s interpreter reference is not resolvable: %v", want.name, err)
		}
	}
}

func TestPredicateReferencesRemainStable(t *testing.T) {
	for _, frozen := range []struct {
		name      string
		reference task.PredicateRef
		digest    string
	}{
		{"read-only v2", readOnlyV2Reference(), "ec70ff51fb9ad100dead3efa5775ac0a2d1ee20374d2872643ba468e8fd2d583"},
		{"read-only v1", LegacyReference(), "e314d5c39aba88f76d104ccc8a91d1265893167866eba1f206d347d67d7d0e45"},
		{"read-only v3", readOnlyV3Reference(), "d99fb715e2c90b413712e860e2a4666bd6687f22a687823ebb236950bbc6b639"},
		{"workspace-write v3", workspaceWriteV3Reference(), "ae54dc7d7985a9a1f4f5f1bf644a6369d88ea4cfda69f7ab310490214fff1a1f"},
		{"read-only v4", readOnlyV4Reference(), "dcb001a0874aadb1dddc034f778b62fbcc5146016f3ecd2ff0c9dca0fe6f11bc"},
		{"read-only v5", readOnlyV5Reference(), "cd9b1fcd05395a13ddda10de45a458bbd1d21200348508214570b6b02797330b"},
		{"workspace-write v4", workspaceWriteV4Reference(), "48f77734d7dcfb2268e4b3b835d9952779026e57bb2fa2d770539fdf4004535d"},
		{"workspace-write v7", workspaceWriteV7Reference(), "2136f5a6611eaa4aaec6f91237b823317b18462046ed29a76c2043abdc632e4f"},
		{"read-only v6", readOnlyV6Reference(), "c46b9cae81cb9ddf21d0882d87df4aaf52fbccce5537c59953d46731d3fdf55c"},
		{"workspace-write v8", workspaceWriteV8Reference(), "6f7c216d7a8bbc7961a8fd7f4dbef1b9c429789ba2d180e4ba052935fd4a382b"},
	} {
		if got := frozen.reference.SHA256; got != frozen.digest {
			t.Fatalf("%s digest changed: %s", frozen.name, got)
		}
	}
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		if got := ReferenceForMode(mode); got.Version != "9" || got.SHA256 != ContractDigest(mode) {
			t.Fatalf("current %s reference is not bound to v9 contract: %+v", mode, got)
		}
		if ReferenceForMode(mode).Equal(priorReferenceForMode(mode)) {
			t.Fatalf("prior fresh-default %s predicate was not retained as distinct historical", mode)
		}
	}
	if workspaceWriteV7Reference().Equal(ReferenceForMode(ModeWorkspaceWrite)) {
		t.Fatal("workspace-write v7 predicate was not retained as historical")
	}
	if strings.Contains(Contract(ModeReadOnly), "Pi workspace-write is not advertised") {
		t.Fatal("current read-only contract retains the historical workspace-write limitation")
	}
}
