package antigravity

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestIdentityObserverDecodesEscapedKeyAndConversationIDAcrossChunks(t *testing.T) {
	count := 0
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		count++
		if identity.Provider != Provider || identity.ConversationID != testConversation {
			t.Fatalf("identity=%+v", identity)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"status":"SUCCESS","conversation_\u0069d":"\u0031` + testConversation[1:] + `","response":"answer"}`)
	for _, b := range data {
		observer.Observe([]byte{b})
	}
	if completeErr := observer.Complete(); completeErr != nil {
		t.Fatal(completeErr)
	}
	if count != 1 {
		t.Fatalf("callback count=%d want=1", count)
	}
}

func TestIdentityObserverDecodesMixedEscapedAndRawStringsAcrossChunks(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{
			name: "raw key escaped first ID rune",
			data: `{"conversation_id":"\u0031` + testConversation[1:] + `"}`,
		},
		{
			name: "escaped key raw ID",
			data: `{"conversation_\u0069d":"` + testConversation + `"}`,
		},
		{
			name: "mixed key and ID escapes",
			data: `{"conversatio\u006e_id":"123e4567-e89b-12d3-a456-42661417\u0034` + `000"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			count := 0
			observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
				count++
				if identity.ConversationID != testConversation {
					t.Fatalf("identity=%+v", identity)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < len(testCase.data); index++ {
				observer.Observe([]byte{testCase.data[index]})
			}
			if completeErr := observer.Complete(); completeErr != nil {
				t.Fatal(completeErr)
			}
			if count != 1 {
				t.Fatalf("callback count=%d want=1", count)
			}
		})
	}
}

func TestIdentityObserverRejectsNonMatchingEscapedStrings(t *testing.T) {
	cases := []string{
		`{"conversation_\u0078d":"` + testConversation + `"}`,
		`{"conversation_\u0069d":"\u007a` + testConversation[1:] + `"}`,
		`{"conversation_id":"123e4567-e89b-12d3-a456-426614174\u007a00"}`,
	}
	for _, data := range cases {
		called := 0
		observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
			called++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < len(data); index++ {
			observer.Observe([]byte{data[index]})
		}
		if completeErr := observer.Complete(); completeErr != nil {
			t.Fatal(completeErr)
		}
		if called != 0 {
			t.Fatalf("nonmatching escaped string produced %d callbacks for %q", called, data)
		}
	}
}

func TestIdentityObserverCompletesOnceAndFreezesLateBytes(t *testing.T) {
	count := 0
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
		count++
		if identity.ConversationID != testConversation {
			t.Fatalf("identity=%+v", identity)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(`{"conversation_id":"` + testConversation + `"}`))
	if completeErr := observer.Complete(); completeErr != nil {
		t.Fatal(completeErr)
	}
	observer.Observe([]byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174001"}`))
	if err := observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("callback count=%d want=1", count)
	}
}

func TestIdentityObserverDoesNotReportPartialEnvelope(t *testing.T) {
	called := false
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(`{"conversation_id":"` + testConversation[:12]))
	if completeErr := observer.Complete(); completeErr != nil {
		t.Fatal(completeErr)
	}
	observer.Observe([]byte(testConversation[12:] + `"}`))
	if called {
		t.Fatal("partial envelope produced an identity")
	}
}

func TestIdentityObserverRejectsMalformedStringEscapesWithoutGuessing(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"conversation_\u0069d":"\ud83d"}`),
		[]byte(`{"conversation_id":"123e4567-e89b-12d3-a456-42661417400\uD"}`),
		[]byte(`{"conversation_id":"123e4567-e89b-12d3-a456-426614174000\q"}`),
		[]byte(`{"conversation_id":"bad","other":1,"conversation_id":"123e4567-e89b-12d3-a456-426614174000"}`),
		[]byte(`{"response":"{\\"conversation_id\\":\\"123e4567-e89b-12d3-a456-426614174000\\"}"}`),
	} {
		called := false
		observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error {
			called = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		observer.Observe(data)
		if err := observer.Complete(); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatalf("malformed or nested string produced identity for %q", data)
		}
	}
}

func TestIdentityObserverHonorsExpectedIDAndCallbackError(t *testing.T) {
	wrong := "123e4567-e89b-12d3-a456-426614174001"
	called := false
	observer, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: wrong}, func(task.SessionIdentity) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(`{"conversation_id":"` + testConversation + `"}`))
	if completeErr := observer.Complete(); completeErr != nil {
		t.Fatal(completeErr)
	}
	if called {
		t.Fatal("unexpected conversation ID was recorded")
	}

	callbackErr := errors.New("record failed")
	observer, err = NewIdentityObserver(testTaskID, task.SessionExpectation{}, func(task.SessionIdentity) error { return callbackErr })
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe([]byte(`{"conversation_id":"` + testConversation + `"}`))
	if err := observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("complete error=%v want=%v", err, callbackErr)
	}
	if err := observer.Complete(); !errors.Is(err, callbackErr) {
		t.Fatalf("repeated complete error=%v want=%v", err, callbackErr)
	}
}

func TestIdentityObserverRejectsInvalidConstruction(t *testing.T) {
	if _, err := NewIdentityObserver("bad", task.SessionExpectation{}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid task ID was accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{Required: true, ID: "bad"}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("invalid expected conversation ID was accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{ID: "unexpected"}, func(task.SessionIdentity) error { return nil }); err == nil {
		t.Fatal("unexpected fresh continuation ID was accepted")
	}
	if _, err := NewIdentityObserver(testTaskID, task.SessionExpectation{}, nil); err == nil {
		t.Fatal("nil identity recorder was accepted")
	}
}
