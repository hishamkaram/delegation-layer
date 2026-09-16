package antigravity

import "testing"

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || len(description.SupportedModes) != 1 || description.SupportedModes[0] != Mode {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatalf("runtime flag requirements are missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 2 {
		t.Fatalf("registration did not expose preparation and interpreters: %+v", registration)
	}
}
