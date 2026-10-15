package lifetime

import (
	"os"
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

	reused, err := NewProcessRef(ref.PID(), ref.StartToken()+"-different")
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
