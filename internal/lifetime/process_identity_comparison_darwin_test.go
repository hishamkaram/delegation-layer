//go:build darwin

package lifetime

import (
	"os"
	"testing"
)

// This observes the actual kernel process, rather than only comparing fabricated
// tokens. Its native macOS result must be obtained on macOS; Linux does not run it.
func TestDarwinProcessPresentUsesStableUniqueID(t *testing.T) {
	observed, err := ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	parts, valid := parseDarwinIdentityToken(observed.StartToken(), "darwin-v2:", 3)
	if !valid {
		t.Fatal("native observer did not return a checked Darwin v2 identity")
	}
	timestamp := "0:0:"
	if parts[0] == "0" && parts[1] == "0" {
		timestamp = "1:1:"
	}
	same, err := NewProcessRef(observed.PID(), "darwin-v2:"+timestamp+parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if present, presentErr := ProcessPresent(same); presentErr != nil || !present {
		t.Fatalf("live native uniqueid refused after timestamp change: %v", presentErr)
	}
	otherID := "1"
	if parts[2] == otherID {
		otherID = "2"
	}
	different, err := NewProcessRef(observed.PID(), "darwin-v2:"+parts[0]+":"+parts[1]+":"+otherID)
	if err != nil {
		t.Fatal(err)
	}
	if present, presentErr := ProcessPresent(different); presentErr != nil || present {
		t.Fatalf("different native uniqueid accepted for same PID: %v", presentErr)
	}
}
