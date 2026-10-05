package claude

import (
	"slices"
	"testing"
)

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || !slices.Contains(description.SupportedModes, Mode) || !slices.Contains(description.SupportedModes, WorkspaceWriteMode) {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatalf("runtime flag requirements are missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 13 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	if !registration.Interpreters[2].Reference().Equal(historicalNativeReferenceForMode(Mode)) ||
		!registration.Interpreters[3].Reference().Equal(historicalNativeReferenceForMode(WorkspaceWriteMode)) ||
		!registration.Interpreters[4].Reference().Equal(legacyNativeReferenceForMode(Mode)) ||
		!registration.Interpreters[5].Reference().Equal(legacyNativeReferenceForMode(WorkspaceWriteMode)) ||
		!registration.Interpreters[6].Reference().Equal(legacyPortableReferenceForMode(Mode)) ||
		!registration.Interpreters[7].Reference().Equal(legacyPortableReferenceForMode(WorkspaceWriteMode)) ||
		!registration.Interpreters[8].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy predicate reference is not registered: %+v", registration.Interpreters[2].Reference())
	}
}

func TestInteractionRegistration(t *testing.T) {
	registration := Registration()
	description := Description()
	if !registration.Interpreters[9].Reference().Equal(interactionReference(Mode)) || !registration.Interpreters[10].Reference().Equal(interactionReference(WorkspaceWriteMode)) ||
		!registration.Interpreters[11].Reference().Equal(historicalInteractionReference(Mode)) || !registration.Interpreters[12].Reference().Equal(historicalInteractionReference(WorkspaceWriteMode)) {
		t.Fatal("interactive predicates missing")
	}
	if !description.Interaction.Clarification || description.Interaction.Steering != false {
		t.Fatalf("wrong interaction capabilities: %+v", description.Interaction)
	}
}
