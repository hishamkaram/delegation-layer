//go:build darwin

package lifetime

import (
	"strings"
	"testing"
)

func TestDarwinBootIDUsesStableKernelSession(t *testing.T) {
	first, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := BootID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "darwin:boot-session:") || CompareBootIDs(first, second) != BootSame {
		t.Fatalf("unstable or invalid boot-session identity: %q, %q", first, second)
	}
	if CompareBootIDs("darwin:1790770768:55074", first) != BootUnknown {
		t.Fatal("legacy wall-clock timestamp was accepted as boot proof")
	}
}
