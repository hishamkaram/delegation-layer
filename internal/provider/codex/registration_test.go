package codex

import "testing"

func TestDescriptionExposesRuntimeCapabilitiesWithoutReleaseGate(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || len(description.SupportedModes) != 1 || description.SupportedModes[0] != Mode {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 || len(description.Runtime.HelpArgs) != 1 || description.Runtime.HelpArgs[0] != "exec" {
		t.Fatalf("runtime capability metadata is missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 1 {
		t.Fatalf("registration did not expose preparation and interpreter: %+v", registration)
	}
}
