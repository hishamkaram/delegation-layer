package codex

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverReportsOneThreadDuringChunkedCapture(t *testing.T) {
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(threadStarted() + "\n{" + `"type":"turn.started"}`)
	for _, byteValue := range data {
		observer.Observe([]byte{byteValue})
	}
	if len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: testThreadID}) {
		t.Fatalf("identity was not reported at thread.started=%+v", got)
	}
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: testThreadID}) {
		t.Fatalf("identities=%+v", got)
	}
	observer.Observe([]byte(`{"type":"thread.started","thread_id":"123e4567-e89b-12d3-a456-426614174001"}`))
	if err = observer.Complete(); err != nil || len(got) != 1 {
		t.Fatalf("late observation changed identity=%+v err=%v", got, err)
	}
}

func TestIdentityObserverDoesNotPersistPartialOrMalformedIdentity(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "partial", data: []byte(`{"type":"thread.started","thread_id":"` + testThreadID[:12])},
		{name: "malformed", data: []byte(`{"type":"thread.started","thread_id":"` + testThreadID + `\q"}`)},
		{name: "invalid UTF-8", data: []byte{0xff}},
		{name: "blank line", data: []byte("\n" + threadStarted())},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
				called = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			observer.Observe(testCase.data)
			if err = observer.Complete(); err != nil {
				t.Fatal(err)
			}
			if called {
				t.Fatal("invalid or conflicting stream persisted an identity")
			}
		})
	}
}

func TestIdentityObserverKeepsTheFirstThreadIdentity(t *testing.T) {
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(threadStarted() + "\n" + `{"type":"thread.started","thread_id":"123e4567-e89b-12d3-a456-426614174001"}`))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ConversationID != testThreadID {
		t.Fatalf("identities=%+v", got)
	}
}

func TestIdentityObserverHonorsExpectedThreadAndCallbackError(t *testing.T) {
	wrong := "123e4567-e89b-12d3-a456-426614174001"
	called := false
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: wrong}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(threadStarted()))
	if err = observer.Complete(); err != nil || called {
		t.Fatalf("wrong expected identity called=%v err=%v", called, err)
	}

	callbackErr := errors.New("record failed")
	observer, err = NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error { return callbackErr })
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(threadStarted()))
	if err = observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("complete error=%v want=%v", err, callbackErr)
	}
	if err = observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("repeated complete error=%v want=%v", err, callbackErr)
	}
}

func TestIdentityObserverConstructionRejectsInvalidInputs(t *testing.T) {
	if _, err := NewIdentityObserver("bad", task.SessionExpectation{}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid task ID accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: "bad"}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid expected thread accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{ID: testThreadID}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("fresh task with continuation ID accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, nil); err == nil {
		t.Fatal("nil recorder accepted")
	}
}
