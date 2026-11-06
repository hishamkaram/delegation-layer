package lifetime

import (
	"os"
	"runtime"
	"testing"
)

func TestObserveProcessMatchesCurrentIdentity(t *testing.T) {
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatalf("ObserveProcess: %v", err)
	}
	present, err := ProcessPresent(ref)
	if err != nil {
		t.Fatalf("ProcessPresent: %v", err)
	}
	if !present {
		t.Fatal("current process was reported absent")
	}

	differentToken := ref.StartToken() + "-different"
	if runtime.GOOS == "darwin" {
		parts, valid := parseDarwinIdentityToken(ref.StartToken(), "darwin-v2:", 3)
		if !valid {
			t.Fatal("native observer did not return a checked Darwin v2 identity")
		}
		otherID := "1"
		if parts[2] == otherID {
			otherID = "2"
		}
		differentToken = "darwin-v2:" + parts[0] + ":" + parts[1] + ":" + otherID
	}
	reused, err := NewProcessRef(ref.PID(), differentToken)
	if err != nil {
		t.Fatalf("NewProcessRef: %v", err)
	}
	present, err = ProcessPresent(reused)
	if err != nil {
		t.Fatalf("ProcessPresent mismatched identity: %v", err)
	}
	if present {
		t.Fatal("mismatched process identity was reported present")
	}
}
