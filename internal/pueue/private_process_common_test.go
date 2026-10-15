package pueue

import (
	"errors"
	"testing"
)

func TestPrivateProcessUnavailableUsesExactIncarnation(t *testing.T) {
	observation := privateProcessObservation{Present: true, StartToken: "native:10"}
	client := &Client{options: Options{processObserver: func(pid int) (privateProcessObservation, error) {
		if pid != 42 {
			t.Fatalf("observed PID %d, want 42", pid)
		}
		return observation, nil
	}}}
	identity := privateProcessIdentity{PID: 42, StartToken: "native:10"}
	unavailable, err := client.privateProcessUnavailable(identity)
	if err != nil || unavailable {
		t.Fatalf("matching process was unavailable: unavailable=%t err=%v", unavailable, err)
	}
	observation.StartToken = "native:11"
	unavailable, err = client.privateProcessUnavailable(identity)
	if err != nil || !unavailable {
		t.Fatalf("reused PID was not unavailable: unavailable=%t err=%v", unavailable, err)
	}
	observation = privateProcessObservation{}
	unavailable, err = client.privateProcessUnavailable(identity)
	if err != nil || !unavailable {
		t.Fatalf("absent process was not unavailable: unavailable=%t err=%v", unavailable, err)
	}

	wantErr := errors.New("inspection unavailable")
	client.options.processObserver = func(int) (privateProcessObservation, error) {
		return privateProcessObservation{}, wantErr
	}
	if _, err = client.privateProcessUnavailable(identity); !errors.Is(err, wantErr) {
		t.Fatalf("process inspection error was not preserved: %v", err)
	}
}
