package claude

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestStreamInterpreterRetainsCallerIdentity(t *testing.T) {
	ref := Reference()
	ref.Adapter, ref.Version, ref.SHA256 = "ccr:print", "test-v1", strings.Repeat("a", 64)
	interpreter, err := NewStreamInterpreter(ref)
	if err != nil {
		t.Fatal(err)
	}
	session, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	stdout := jsonl(initLine(session), resultLine(session, "answer"))
	input := claudeInput(stdout)
	input.Seal.Predicate = ref
	input.RecordedSession = &task.SessionIdentity{Provider: "ccr:print", ConversationID: session}
	var answer bytes.Buffer
	result, err := interpreter.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || result.Session == nil || result.Session.Provider != "ccr:print" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	input.RecordedSession.Provider = Provider
	answer.Reset()
	result, err = interpreter.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalIdentity || answer.Len() != 0 {
		t.Fatalf("cross-provider identity accepted: %+v %v", result, err)
	}
	input.RecordedSession.Provider = "ccr:print"
	input.Seal.ExitCode = 1
	result, err = interpreter.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictRejected {
		t.Fatalf("outer failure accepted: %+v %v", result, err)
	}
}

func TestStreamObserverRecordsCallerIdentityAcrossChunks(t *testing.T) {
	session, err := FreshSessionID(claudeTestRootID, claudeTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded task.SessionIdentity
	observer, err := NewStreamIdentityObserver("ccr:print", claudeTestRootID, claudeTestTaskID, task.SessionExpectation{}, func(identity task.SessionIdentity) error { recorded = identity; return nil })
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(initLine(session) + "\n")
	for _, value := range data {
		observer.Observe([]byte{value})
	}
	if err := observer.Complete(); err != nil {
		t.Fatal(err)
	}
	if recorded.Provider != "ccr:print" || recorded.ConversationID != session {
		t.Fatalf("identity=%+v", recorded)
	}
}
