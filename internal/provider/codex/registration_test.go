package codex

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || !slices.Contains(description.SupportedModes, Mode) || !slices.Contains(description.SupportedModes, WorkspaceWriteMode) {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 || len(description.Runtime.HelpArgs) != 1 || description.Runtime.HelpArgs[0] != "exec" {
		t.Fatalf("runtime capability metadata is missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || registration.PrepareExisting == nil {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatalf("invalid or duplicate registered reference: %v", err)
	}
	for _, ref := range []task.PredicateRef{
		ReferenceForMode(Mode), ReferenceForMode(WorkspaceWriteMode),
		historicalV3ReferenceForMode(Mode), historicalV3ReferenceForMode(WorkspaceWriteMode),
		historicalCurrentRuntimeReferenceForMode(Mode), historicalCurrentRuntimeReferenceForMode(WorkspaceWriteMode),
		historicalRuntimeReferenceForMode(Mode), historicalRuntimeReferenceForMode(WorkspaceWriteMode),
		LegacyReference(),
	} {
		if _, err := registry.Resolve(ref); err != nil {
			t.Fatalf("required predicate %+v is not resolvable: %v", ref, err)
		}
	}
}

func TestInteractionRegistration(t *testing.T) {
	registration := Registration()
	description := Description()
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []task.PredicateRef{
		interactionReference(Mode), interactionReference(WorkspaceWriteMode),
		strictInteractionReference(Mode), strictInteractionReference(WorkspaceWriteMode),
		historicalInteractionReference(Mode), historicalInteractionReference(WorkspaceWriteMode),
	} {
		if _, err := registry.Resolve(ref); err != nil {
			t.Fatalf("interactive predicate %+v is not resolvable: %v", ref, err)
		}
	}
	if !description.Interaction.Clarification || description.Interaction.Steering != true {
		t.Fatalf("wrong interaction capabilities: %+v", description.Interaction)
	}
}

func TestReviewRegistrationIsReadOnlyAndNonResumable(t *testing.T) {
	registration := ReviewRegistration()
	if registration.Description.ID != ReviewProvider || registration.Prepare == nil || registration.PrepareExisting == nil {
		t.Fatalf("review registration=%+v", registration)
	}
	if !slices.Contains(registration.Description.SupportedModes, Mode) || registration.Description.Continuation != commonprovider.ContinuationUnsupported {
		t.Fatalf("review capabilities=%+v", registration.Description)
	}
	if !slices.Contains(registration.Description.RequiredOptions, commonprovider.OptionReviewTarget) {
		t.Fatalf("review target contract=%+v", registration.Description)
	}
	registry, err := predicate.New(registration.Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(reviewReference()); err != nil {
		t.Fatalf("review predicate is not resolvable: %v", err)
	}
	for _, interpreter := range registration.Interpreters {
		ref := interpreter.Reference()
		if ref.Adapter != ReviewProvider || ref.Mode != Mode {
			t.Fatalf("review registration includes an incompatible predicate: %+v", ref)
		}
	}
}
