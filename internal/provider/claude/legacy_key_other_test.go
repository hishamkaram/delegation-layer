//go:build !darwin

package claude

import (
	"errors"
	"testing"
)

func TestLegacyAPIKeyAbsenceIsUnsupportedOutsideDarwin(t *testing.T) {
	if err := inspectLegacyAPIKeyAbsence(profileEnvironment{}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("got %v", err)
	}
}
