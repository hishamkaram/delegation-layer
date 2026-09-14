package contributorprovider

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverRecordsExactSessionOnce(t *testing.T) {
	const taskID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, resume := range []bool{false, true} {
		expected := task.SessionExpectation{}
		session := "session-" + taskID
		if resume {
			session = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			expected = task.SessionExpectation{Required: true, ID: session}
		}
		calls := 0
		observer, err := identityFactory(taskID)(expected, func(identity task.SessionIdentity) error {
			calls++
			if identity.Provider != Provider || identity.ConversationID != session {
				t.Fatalf("unexpected identity: %+v", identity)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		data := identityEnvelope(t, taskID, session)
		for _, b := range data {
			observer.Observe([]byte{b})
		}
		if calls != 0 {
			t.Fatal("identity recorded before complete envelope")
		}
		for range 2 {
			if err := observer.Complete(); err != nil {
				t.Fatal(err)
			}
			observer.Observe(data)
		}
		if calls != 1 {
			t.Fatalf("recorded %d times", calls)
		}
	}
}

func TestIdentityObserverInvalidEvidenceDoesNotRecord(t *testing.T) {
	const taskID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	valid := identityEnvelope(t, taskID, "session-"+taskID)
	cases := map[string][]byte{
		"truncated":        valid[:len(valid)-2],
		"trailing":         append(append([]byte{}, valid...), valid...),
		"task-mismatch":    identityEnvelope(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "session-"+taskID),
		"session-mismatch": identityEnvelope(t, taskID, "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		"legacy-session":   identityEnvelope(t, taskID, "legacy-session"),
		"oversized":        []byte(strings.Repeat(" ", MaxEnvelopeBytes+1)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			observer, err := identityFactory(taskID)(task.SessionExpectation{}, func(task.SessionIdentity) error {
				t.Fatal("invalid identity recorded")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			observer.Observe(data)
			observer.Observe(valid)
			if err := observer.Complete(); err != nil {
				t.Fatalf("semantic fault became observer failure: %v", err)
			}
		})
	}
}

func TestIdentityObserverRecorderFailureIsRetained(t *testing.T) {
	const taskID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	want := errors.New("persistence failure")
	calls := 0
	observer, err := identityFactory(taskID)(task.SessionExpectation{}, func(task.SessionIdentity) error {
		calls++
		return want
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe(identityEnvelope(t, taskID, "session-"+taskID))
	for range 2 {
		if err := observer.Complete(); !errors.Is(err, want) {
			t.Fatalf("callback failure lost: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("failed callback retried %d times", calls)
	}
}

func TestIdentityObserverRejectsInvalidConstruction(t *testing.T) {
	const taskID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	record := func(task.SessionIdentity) error { return nil }
	for _, expected := range []task.SessionExpectation{
		{Required: true},
		{ID: "unexpected"},
		{Required: true, ID: "legacy-session"},
		{Required: true, ID: "session-" + strings.Repeat("a", 31)},
	} {
		if _, err := identityFactory(taskID)(expected, record); err == nil {
			t.Fatalf("accepted invalid expectation: %+v", expected)
		}
	}
	if _, err := identityFactory("invalid")(task.SessionExpectation{}, record); err == nil {
		t.Fatal("accepted invalid task ID")
	}
	if _, err := identityFactory(taskID)(task.SessionExpectation{}, nil); err == nil {
		t.Fatal("accepted nil recorder")
	}
}

func identityEnvelope(t *testing.T, taskID, sessionID string) []byte {
	t.Helper()
	data, err := task.MarshalCanonical(Envelope{Protocol: Protocol, TaskID: taskID, SessionID: sessionID, Status: "complete", Answer: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
