package task

import (
	"strings"
	"testing"
)

func TestRemoteBindingParticipatesInRequestEquality(t *testing.T) {
	a := &TaskRecord{}
	b := &TaskRecord{}
	if !CompareRequests(a, b) {
		t.Fatal("historical absent bindings differ")
	}
	a.Remote = &RemoteBinding{OriginRootID: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64), OutputPaths: []string{"result.txt"}}
	if CompareRequests(a, b) {
		t.Fatal("remote binding ignored")
	}
	b.Remote = a.Remote.Clone()
	if !CompareRequests(a, b) {
		t.Fatal("matching bindings differ")
	}
	b.Remote.OutputPaths[0] = "different.txt"
	if CompareRequests(a, b) {
		t.Fatal("output selection not immutable")
	}
	if a.Remote.OutputPaths[0] != "result.txt" {
		t.Fatal("clone shares output storage")
	}
}

func TestRemoteBindingRejectsUnsafeOutputs(t *testing.T) {
	for _, output := range []string{"../secret", "/secret", "a/../secret", ".", "a\\b"} {
		binding := &RemoteBinding{OriginRootID: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64), OutputPaths: []string{output}}
		if err := validateRemoteBinding(binding); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
}
