package claude

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type claudeRecordObserver interface {
	ObserveRecord(providerjson.Value) error
	Complete() error
}

func newRecordIdentityObserver(t *testing.T, record func(task.SessionIdentity) error) claudeRecordObserver {
	t.Helper()
	observer, err := NewIdentityObserver(claudeTestRootID, claudeTestTaskID, task.SessionExpectation{Required: true, ID: claudeTestUUID}, record)
	if err != nil {
		t.Fatal(err)
	}
	recordObserver, ok := observer.(claudeRecordObserver)
	if !ok {
		t.Fatal("Claude observer does not observe records")
	}
	return recordObserver
}

func observeClaudeRecord(observer claudeRecordObserver, line string) error {
	return observer.ObserveRecord(providerjson.Value{Reader: strings.NewReader(line), End: int64(len(line))})
}

func TestRecordIdentityObserverRecordsRepeatedSameSessionInitOnce(t *testing.T) {
	var got []task.SessionIdentity
	observer := newRecordIdentityObserver(t, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	for _, line := range nativeFollowUpTrace() {
		if err := observeClaudeRecord(observer, line); err != nil {
			t.Fatalf("observe record: %v", err)
		}
	}
	if err := observeClaudeRecord(observer, writeInitLine("123e4567-e89b-12d3-a456-426614174001")); err == nil {
		t.Fatal("conflicting repeated init was accepted")
	}
	if err := observer.Complete(); err != nil || len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) {
		t.Fatalf("identities=%+v err=%v", got, err)
	}
}

func TestRecordIdentityObserverKeepsRecorderFailureOperational(t *testing.T) {
	recorderErr := errors.New("session store unavailable")
	calls := 0
	observer := newRecordIdentityObserver(t, func(task.SessionIdentity) error {
		calls++
		return recorderErr
	})
	if err := observeClaudeRecord(observer, writeInitLine(claudeTestUUID)); !errors.Is(err, recorderErr) {
		t.Fatalf("first init error=%v", err)
	}
	if err := observeClaudeRecord(observer, writeInitLine(claudeTestUUID)); !errors.Is(err, recorderErr) {
		t.Fatalf("repeated init hid the recorder failure: %v", err)
	}
	if err := observer.Complete(); !errors.Is(err, recorderErr) || calls != 1 {
		t.Fatalf("complete=%v calls=%d", err, calls)
	}
}
