package antigravity

import "testing"

func TestDescriptionExposesRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable || len(description.SupportedModes) != 2 || description.SupportedModes[0] != ModeReadOnly || description.SupportedModes[1] != ModeWorkspaceWrite {
		t.Fatalf("unexpected description: %+v", description)
	}
	if len(description.Runtime.RequiredFlags) == 0 {
		t.Fatalf("runtime flag requirements are missing: %+v", description)
	}
	registration := Registration()
	if registration.Prepare == nil || len(registration.Interpreters) != 9 {
		t.Fatalf("registration did not expose preparation and interpreters: %+v", registration)
	}
	seen := make(map[string]bool)
	for _, interpreter := range registration.Interpreters {
		seen[interpreter.Reference().Version] = true
	}
	for _, version := range []string{"1.2.2", "1.2.2-auth2", "1.2.2-auth3", "print-record-v1", "print-record-v2"} {
		if !seen[version] {
			t.Fatalf("predicate revision %q was not registered: %+v", version, registration)
		}
	}
}
