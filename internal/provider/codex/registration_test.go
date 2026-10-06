package codex

import (
	"slices"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 11 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	if !registration.Interpreters[6].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy predicate reference is not registered: %+v", registration.Interpreters[6].Reference())
	}
}

func TestInteractionRegistration(t *testing.T) {
	registration := Registration()
	description := Description()
	if !registration.Interpreters[7].Reference().Equal(interactionReference(Mode)) || !registration.Interpreters[8].Reference().Equal(interactionReference(WorkspaceWriteMode)) {
		t.Fatal("interactive predicates missing")
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
	if !slices.Contains(registration.Description.RequiredOptions, commonprovider.OptionReviewTarget) || len(registration.Interpreters) != 1 {
		t.Fatalf("review target contract=%+v", registration.Description)
	}
	if !registration.Interpreters[0].Reference().Equal(reviewReference()) {
		t.Fatalf("review predicate=%+v", registration.Interpreters[0].Reference())
	}
}
