package infrastructure

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func savedBindingSourceFixture(t *testing.T, kind SavedBindingKind) (SavedBindingSource, task.SupervisorRef) {
	t.Helper()
	binding := task.SupervisorRef{
		ClientExecutable: "/custody/client", DaemonExecutable: "/custody/daemon",
		ClientSHA256: strings.Repeat("c", 64), DaemonSHA256: strings.Repeat("d", 64),
		ConfigPath: "/state/supervisor.yml", ConfigDigest: strings.Repeat("e", 64),
		Endpoint: "unix:/state/supervisor.socket", ObservedVersion: "4.0.0",
	}
	digest, err := SavedBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	source := SavedBindingSource{RootID: strings.Repeat("a", 32), Kind: kind, OwnerID: strings.Repeat("b", 32), BindingSHA256: digest}
	if kind == SavedBindingTaskStop {
		source.RecordID = strings.Repeat("c", 32)
	}
	return source, binding
}

func TestSavedBindingSourceClosedSlots(t *testing.T) {
	t.Run("canonical budget stop slot", func(t *testing.T) {
		source, binding := savedBindingSourceFixture(t, SavedBindingTaskStop)
		source.RecordID = "budget"
		if err := task.ValidateStopRequestID(source.RecordID); err != nil {
			t.Fatal(err)
		}
		if err := source.ValidateBinding(binding); err != nil {
			t.Fatalf("canonical stop request ID refused: %v", err)
		}
	})
	for _, kind := range []SavedBindingKind{SavedBindingTaskMetadata, SavedBindingTaskSubmission, SavedBindingTaskStop, SavedBindingWorkspaceRequest} {
		t.Run(string(kind), func(t *testing.T) {
			source, binding := savedBindingSourceFixture(t, kind)
			if err := source.ValidateBinding(binding); err != nil {
				t.Fatal(err)
			}
			invalid := source
			if kind == SavedBindingTaskStop {
				invalid.RecordID = ""
			} else {
				invalid.RecordID = strings.Repeat("c", 32)
			}
			if err := invalid.Validate(); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid slot accepted: %v", err)
			}
		})
	}
	for _, corrupt := range []func(*SavedBindingSource){
		func(s *SavedBindingSource) { s.RootID = "" },
		func(s *SavedBindingSource) { s.OwnerID = "../other" },
		func(s *SavedBindingSource) { s.BindingSHA256 = "" },
		func(s *SavedBindingSource) { s.Kind = "inspectionrequest" },
		func(s *SavedBindingSource) { s.Kind = "rootpublication" },
		func(s *SavedBindingSource) { s.Kind = "" },
	} {
		source, _ := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
		corrupt(&source)
		if err := source.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid source accepted: %v", err)
		}
	}
}

func TestSavedBindingSourceBindsWholeValue(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*task.SupervisorRef)
	}{
		{"client path", func(b *task.SupervisorRef) { b.ClientExecutable = "/other/client" }},
		{"daemon path", func(b *task.SupervisorRef) { b.DaemonExecutable = "/other/daemon" }},
		{"client digest", func(b *task.SupervisorRef) { b.ClientSHA256 = strings.Repeat("1", 64) }},
		{"daemon digest", func(b *task.SupervisorRef) { b.DaemonSHA256 = strings.Repeat("2", 64) }},
		{"configuration path", func(b *task.SupervisorRef) { b.ConfigPath = "/other/config.yml" }},
		{"configuration digest", func(b *task.SupervisorRef) { b.ConfigDigest = strings.Repeat("3", 64) }},
		{"resolved configuration", func(b *task.SupervisorRef) { b.ResolvedConfigSHA256 = strings.Repeat("4", 64) }},
		{"endpoint", func(b *task.SupervisorRef) { b.Endpoint = "unix:/other/socket" }},
		{"observed version", func(b *task.SupervisorRef) { b.ObservedVersion = "4.0.1" }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source, binding := savedBindingSourceFixture(t, SavedBindingTaskSubmission)
			fixture.change(&binding)
			if err := task.ValidateSupervisorRef(binding); err != nil {
				t.Fatalf("changed fixture is not independently valid: %v", err)
			}
			if err := source.ValidateBinding(binding); !errors.Is(err, ErrIdentity) {
				t.Fatalf("changed complete binding accepted: %v", err)
			}
		})
	}
	source, binding := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
	binding.ConfigPath = "../untrusted"
	if _, err := SavedBindingDigest(binding); !errors.Is(err, ErrIdentity) {
		t.Fatalf("invalid binding received a digest: %v", err)
	}
	if err := source.ValidateBinding(binding); !errors.Is(err, ErrIdentity) {
		t.Fatalf("invalid binding accepted: %v", err)
	}
}
