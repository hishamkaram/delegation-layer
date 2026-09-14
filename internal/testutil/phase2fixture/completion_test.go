package phase2fixture

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverCompleteFreezesEmptyStream(t *testing.T) {
	calls := 0
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionFrame(testTaskID, "conv-1")))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("identity callbacks after repeated Complete = %d, want 0", calls)
	}
}

func TestIdentityObserverCompleteFreezesPartialFrame(t *testing.T) {
	calls := 0
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	partial := sessionFrame(testTaskID, "conv-1")
	observer.Observe([]byte(partial[:len(partial)-1]))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(sessionFrame(testTaskID, "conv-1")))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("identity callbacks after partial frame = %d, want 0", calls)
	}
}
