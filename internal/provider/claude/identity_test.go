package claude

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	claudeTestRootID = "fedcba9876543210fedcba9876543210"
	claudeTestTaskID = "0123456789abcdef0123456789abcdef"
	claudeTestUUID   = "123e4567-e89b-12d3-a456-426614174000"
)

func TestFreshSessionIDIsDeterministicUUIDv5(t *testing.T) {
	got, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	const want = "99b93a5f-a02f-5975-8eea-ea1dd72facb2"
	if got != want || !validSessionID(got) {
		t.Fatalf("fresh session=%q want=%q", got, want)
	}
	if gotAgain, err := FreshSessionID(claudeTestRootID, claudeTestTaskID); err != nil || gotAgain != got {
		t.Fatalf("fresh session is not repeatable: %q %v", gotAgain, err)
	}
	if gotOther, err := FreshSessionID(claudeTestRootID, "abcdef0123456789abcdef0123456789"); err != nil || gotOther == got {
		t.Fatalf("task name did not bind UUID: %q %v", gotOther, err)
	}
}

func TestIdentityObserverReportsExpectedInitDuringChunkedCapture(t *testing.T) {
	fresh, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	var got []task.SessionIdentity
	observer, err := NewIdentityObserver(claudeTestRootID, claudeTestTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(initLine(fresh) + "\n{" + `"type":"assistant"}`)
	for _, byteValue := range data {
		observer.Observe([]byte{byteValue})
	}
	if len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: fresh}) {
		t.Fatalf("identity was not reported at system/init=%+v", got)
	}
	if err := observer.Complete(); err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(initLine(fresh)))
	if err := observer.Complete(); err != nil || len(got) != 1 {
		t.Fatalf("late observation changed identity=%+v err=%v", got, err)
	}
}

func TestIdentityObserverDoesNotRecordMalformedOrWrongIdentity(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "partial", data: []byte(`{"type":"system","subtype":"init","session_id":"` + claudeTestUUID[:12])},
		{name: "malformed", data: []byte(`{"type":"system","subtype":"init","session_id":"` + claudeTestUUID + `\q"}`)},
		{name: "invalid UTF-8", data: []byte{0xff}},
		{name: "blank line", data: []byte("\n" + initLine(claudeTestUUID))},
		{name: "oversized", data: append(bytes.Repeat([]byte{'x'}, maxEventLineBytes+1), '\n')},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			observer, err := NewIdentityObserver(claudeTestRootID, claudeTestTaskID, task.SessionExpectation{Required: true, ID: claudeTestUUID}, func(task.SessionIdentity) error {
				called = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			observer.Observe(testCase.data)
			if err := observer.Complete(); err != nil {
				t.Fatal(err)
			}
			if called {
				t.Fatal("invalid identity was recorded")
			}
		})
	}
}

func TestIdentityObserverHonorsExpectationAndCallbackError(t *testing.T) {
	fresh, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	observer, err := NewIdentityObserver(claudeTestRootID, claudeTestTaskID, task.SessionExpectation{Required: true, ID: claudeTestUUID}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(initLine(fresh)))
	completeErr := observer.Complete()
	if completeErr != nil || called {
		t.Fatalf("wrong expected identity called=%v err=%v", called, completeErr)
	}

	callbackErr := errors.New("record failed")
	observer, err = NewIdentityObserver(claudeTestRootID, claudeTestTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error { return callbackErr })
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(initLine(fresh)))
	completeErr = observer.Complete()
	if !errors.Is(completeErr, callbackErr) {
		t.Fatalf("complete error=%v want=%v", completeErr, callbackErr)
	}
	if err := observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("repeated complete error=%v want=%v", err, callbackErr)
	}
}

func TestIdentityObserverConstructionRejectsInvalidInputs(t *testing.T) {
	recorder := func(task.SessionIdentity) error { return nil }
	for name, args := range map[string]struct {
		root     string
		taskID   string
		expect   task.SessionExpectation
		recorder func(task.SessionIdentity) error
	}{
		"root":        {root: "bad", taskID: claudeTestTaskID, recorder: recorder},
		"task":        {root: claudeTestRootID, taskID: "bad", recorder: recorder},
		"session":     {root: claudeTestRootID, taskID: claudeTestTaskID, expect: task.SessionExpectation{Required: true, ID: "bad"}, recorder: recorder},
		"fresh alias": {root: claudeTestRootID, taskID: claudeTestTaskID, expect: task.SessionExpectation{ID: claudeTestUUID}, recorder: recorder},
		"recorder":    {root: claudeTestRootID, taskID: claudeTestTaskID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIdentityObserver(args.root, args.taskID, args.expect, args.recorder); err == nil {
				t.Fatal("invalid observer inputs accepted")
			}
		})
	}
}

func initLine(sessionID string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":%q,"apiKeySource":"none","cwd":"/workspace","tools":["Read","Glob","Grep"],"mcp_servers":[],"model":"claude-test","permissionMode":"dontAsk"}`, sessionID, Version)
}

func writeInitLine(sessionID string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":%q,"apiKeySource":"none","cwd":"/workspace","tools":["Read","Edit","Write","Glob","Grep"],"mcp_servers":[],"model":"claude-test","permissionMode":"acceptEdits"}`, sessionID, Version)
}

func jsonl(lines ...string) []byte { return []byte(strings.Join(lines, "\n")) }
