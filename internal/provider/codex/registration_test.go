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
	if registration.Prepare == nil || registration.PrepareExisting == nil || len(registration.Interpreters) != 3 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
	if !registration.Interpreters[2].Reference().Equal(LegacyReference()) {
		t.Fatalf("legacy predicate reference is not registered: %+v", registration.Interpreters[2].Reference())
	}
}
