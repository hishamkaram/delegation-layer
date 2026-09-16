package opencode

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverRecordsFreshSessionAcrossChunkedCapture(t *testing.T) {
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(stepStart(testSession) + "\n" + textEvent(testSession, "answer"))
	for _, value := range data {
		observer.Observe([]byte{value})
	}
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: testSession}) {
		t.Fatalf("identity=%+v", got)
	}
	observer.Observe([]byte(textEvent(testSession, "late")))
	if err = observer.Complete(); err != nil || len(got) != 1 {
		t.Fatalf("late observation changed identity=%+v err=%v", got, err)
	}
}

func TestIdentityObserverHonorsExpectedSessionAndRejectsConflicts(t *testing.T) {
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: testSession}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(stepStart(testSession)))
	if err = observer.Complete(); err != nil || len(got) != 1 {
		t.Fatalf("expected identity=%+v err=%v", got, err)
	}

	observer, err = NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(stepStart(testSession) + "\n" + stepStart("ses_other_123")))
	if err = observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("conflicting stream emitted unexpected callback count=%d", len(got))
	}
}

func TestIdentityObserverConstructionValidation(t *testing.T) {
	cases := []struct {
		name     string
		expected task.SessionExpectation
		record   func(task.SessionIdentity) error
	}{
		{name: "invalid task", expected: task.SessionExpectation{}, record: func(task.SessionIdentity) error { return nil }},
		{name: "invalid expected", expected: task.SessionExpectation{Required: true, ID: "latest"}, record: func(task.SessionIdentity) error { return nil }},
		{name: "unexpected fresh id", expected: task.SessionExpectation{ID: testSession}, record: func(task.SessionIdentity) error { return nil }},
		{name: "nil recorder", expected: task.SessionExpectation{}, record: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			taskID := testTaskID
			if testCase.name == "invalid task" {
				taskID = "invalid"
			}
			if _, err := NewIdentityObserver(taskID, testCase.expected, testCase.record); err == nil {
				t.Fatal("invalid identity observer construction was accepted")
			}
		})
	}

	callbackErr := errors.New("record failed")
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error { return callbackErr })
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(stepStart(testSession)))
	if err = observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("complete error=%v want=%v", err, callbackErr)
	}
}
