package testsupport

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

func CurrentBootID(t *testing.T) string {
	t.Helper()
	value, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// A different spelling is not reboot evidence. Use a comparable kernel UUID
// scheme, and verify the fixture actually represents a different boot.
func DifferentNativeBootID(t *testing.T) string {
	t.Helper()
	current := CurrentBootID(t)
	prefix := "linux:"
	if strings.HasPrefix(current, "darwin:boot-session:") {
		prefix = "darwin:boot-session:"
	}
	candidate := prefix + "00000000-0000-0000-0000-000000000001"
	if candidate == current {
		candidate = prefix + "00000000-0000-0000-0000-000000000002"
	}
	if lifetime.CompareBootIDs(candidate, current) != lifetime.BootDifferent {
		t.Fatalf("not a comparable prior boot fixture: %q, %q", candidate, current)
	}
	return candidate
}
