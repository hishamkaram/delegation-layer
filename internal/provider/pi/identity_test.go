package pi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func sessionLine(id string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":"/workspace"}`, id)
}

func TestIdentityObserverRecordsFreshSessionFromChunkedHeader(t *testing.T) {
	var recorded []task.SessionIdentity
	observer, err := NewIdentityObserver(task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		recorded = append(recorded, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := sessionLine(testUUID) + "\n{\"type\":\"agent_start\"}"
	for index := range data {
		observer.Observe([]byte{data[index]})
	}
	if err := observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0] != (task.SessionIdentity{Provider: Provider, ConversationID: testUUID}) {
		t.Fatalf("recorded=%+v", recorded)
	}
}

func TestIdentityObserverRequiresExactContinuation(t *testing.T) {
	called := false
	observer, err := NewIdentityObserver(task.SessionExpectation{Required: true, ID: testUUID}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionLine("123e4567-e89b-12d3-a456-426614174001")))
	if err := observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("recorded a nonmatching continuation")
	}
}

func TestIdentityObserverPropagatesRecorderFailure(t *testing.T) {
	want := errors.New("record failed")
	observer, err := NewIdentityObserver(task.SessionExpectation{}, func(task.SessionIdentity) error { return want })
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionLine(testUUID)))
	if err := observer.Complete(); !errors.Is(err, want) {
		t.Fatalf("complete error=%v", err)
	}
}

func TestIdentityObserverRejectsInvalidConstruction(t *testing.T) {
	for name, expected := range map[string]task.SessionExpectation{
		"bad required ID":        {Required: true, ID: "latest"},
		"unexpected optional ID": {ID: testUUID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIdentityObserver(expected, func(task.SessionIdentity) error { return nil }); err == nil {
				t.Fatal("invalid expectation accepted")
			}
		})
	}
	if _, err := NewIdentityObserver(task.SessionExpectation{}, nil); err == nil {
		t.Fatal("nil recorder accepted")
	}
}
