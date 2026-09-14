package phase2fixture

import (
	"errors"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverReportsOneLateSessionAndDrainsAfterFault(t *testing.T) {
	var mu sync.Mutex
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: "conv-1"}, func(identity task.SessionIdentity) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout := answerFrame(testTaskID, "before session") + sessionFrame(testTaskID, "conv-1") + resultFrame(testTaskID, "conv-1", true) + sessionFrame(testTaskID, "conv-1")
	for _, chunk := range splitBytes([]byte(stdout), 3) {
		observer.Observe(chunk)
	}
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: "conv-1"}) {
		t.Fatalf("identities = %+v", got)
	}
}

func TestIdentityObserverCallbackErrorIsReportedOnce(t *testing.T) {
	callbackErr := errors.New("identity storage failed")
	calls := 0
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
		calls++
		return callbackErr
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionFrame(testTaskID, "conv-1")))
	observer.Observe([]byte(sessionFrame(testTaskID, "conv-1")))
	if err = observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("complete error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("callback calls=%d, want 1", calls)
	}
	if err = observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("second complete error=%v", err)
	}
}

func TestIdentityObserverDoesNotGuessFromResultOrReplaceIdentity(t *testing.T) {
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(resultFrame(testTaskID, "conv-1", true)))
	observer.Observe([]byte(sessionFrame(testTaskID, "conv-1")))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("result-only identity callback = %+v", got)
	}

	observer, err = NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionFrame(testTaskID, "first")))
	observer.Observe([]byte(sessionFrame(testTaskID, "second")))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ConversationID != "first" {
		t.Fatalf("conflicting identities = %+v", got)
	}
}

func TestIdentityObserverRejectsInvalidConstructionAndSourcePrefix(t *testing.T) {
	if _, err := NewIdentityObserver("bad", task.SessionExpectation{}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid task ID accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: "bad/id"}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid required session accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, nil); err == nil {
		t.Fatal("nil recorder accepted")
	}

	called := false
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(`{"type":"session","task_id":"` + testTaskID + `","session_id":"conv-1"}`))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unterminated session invoked callback")
	}
}

func splitBytes(data []byte, size int) [][]byte {
	result := make([][]byte, 0, (len(data)+size-1)/size)
	for len(data) > 0 {
		n := size
		if n > len(data) {
			n = len(data)
		}
		result = append(result, append([]byte(nil), data[:n]...))
		data = data[n:]
	}
	return result
}
