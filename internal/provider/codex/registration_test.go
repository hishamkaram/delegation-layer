package codex

import (
	"slices"
	"testing"
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
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 9 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	if !registration.Interpreters[4].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy predicate reference is not registered: %+v", registration.Interpreters[4].Reference())
	}
}

func TestInteractionRegistration(t *testing.T) {
	registration := Registration()
	description := Description()
	if !registration.Interpreters[5].Reference().Equal(interactionReference(Mode)) || !registration.Interpreters[6].Reference().Equal(interactionReference(WorkspaceWriteMode)) {
		t.Fatal("interactive predicates missing")
	}
	if !description.Interaction.Clarification || description.Interaction.Steering != true {
		t.Fatalf("wrong interaction capabilities: %+v", description.Interaction)
	}
}
