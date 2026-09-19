//go:build !darwin

package claude

import "testing"

func TestLegacyAPIKeyAbsenceIsOptionalOutsideDarwin(t *testing.T) {
	if err := inspectLegacyAPIKeyAbsence(profileEnvironment{}); err != nil {
		t.Fatalf("optional legacy check failed: %v", err)
	}
	if result := inspectLegacyAPIKey(profileEnvironment{}); !result.Verified || result.Err != nil {
		t.Fatalf("optional legacy check was not treated as absent: %+v", result)
	}
}
