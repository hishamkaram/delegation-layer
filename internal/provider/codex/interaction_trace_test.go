package codex

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const traceAnswer = "root answer"

// nativeSetupTrace is the stdout order observed from Codex 0.163.0: metadata
// notifications, the thread/start response, the standard thread/started
// notification, startup warnings, and only then the turn/start response.
func nativeSetupTrace() []string {
	handled := handledSetupTrace()
	started := fmt.Sprintf(`{"method":"thread/started","params":{"thread":{"id":%q,"preview":"","status":{"type":"idle"}}}}`, testThreadID)
	return joinTrace(handled[:4], []string{started}, handled[4:])
}

// handledSetupTrace is nativeSetupTrace without the standard thread/started
// notification, which interactive-v2 treats as a protocol fault.
func handledSetupTrace() []string {
	return []string{
		`{"id":"delegate-init","result":{"userAgent":"codex/0.163.0","codexHome":"/home/fixture/.codex"}}`,
		`{"method":"configWarning","params":{"summary":"fixture warning","details":null}}`,
		`{"method":"remoteControl/status/changed","params":{"status":"disabled","environmentId":null}}`,
		fmt.Sprintf(`{"id":"delegate-thread","result":{"thread":{"id":%q,"preview":"","status":{"type":"idle"}},"model":"fixture"}}`, testThreadID),
		`{"method":"warning","params":{"message":"fixture warning"}}`,
		`{"method":"mcpServer/startupStatus/updated","params":{"name":"fixture","status":"starting","error":null}}`,
		`{"id":"delegate-turn","result":{"turn":{"id":"turn-1","status":"inProgress","items":[]}}}`,
	}
}

func rootNotification(method, fields string) string {
	return fmt.Sprintf(`{"method":%q,"params":{"threadId":%q%s}}`, method, testThreadID, fields)
}

// progressTrace is the standard notification traffic of one root turn that
// the interactive-v2 parser treats as a protocol fault.
func progressTrace() []string {
	return []string{
		rootNotification("item/started", `,"turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":""}`),
		rootNotification("item/agentMessage/delta", `,"turnId":"turn-1","itemId":"item-1","delta":"root"`),
		rootNotification("thread/tokenUsage/updated", `,"turnId":"turn-1","tokenUsage":{"total":{"totalTokens":3}}`),
		rootNotification("turn/diff/updated", `,"turnId":"turn-1","diff":""`),
		rootNotification("turn/plan/updated", `,"turnId":"turn-1","plan":[]`),
	}
}

// coreTurnTrace is the handled subset: start, one final root message, and a
// successful completion.
func coreTurnTrace() []string {
	return []string{
		rootNotification("turn/started", `,"turn":{"id":"turn-1","status":"inProgress","items":[]}`),
		finalMessage("item-1", traceAnswer),
		rootTurnCompleted("turn-1", `"status":"completed"`),
	}
}

func standardTurnTrace() []string {
	core := coreTurnTrace()
	return joinTrace(core[:1], progressTrace(), core[1:])
}

func finalMessage(itemID, text string) string {
	return rootNotification("item/completed", fmt.Sprintf(`,"turnId":"turn-1","item":{"type":"agentMessage","id":%q,"phase":"final_answer","text":%q}`, itemID, text))
}

func rootTurnCompleted(turnID, status string) string {
	return fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":%q,"turn":{"id":%q,%s}}}`, testThreadID, turnID, status)
}

func joinTrace(parts ...[]string) []string {
	var lines []string
	for _, part := range parts {
		lines = append(lines, part...)
	}
	return lines
}

// traceRun is one offline replay of recorded stdout against one revision.
type traceRun struct {
	revision interactionRevision
	lines    []string
	expected task.SessionExpectation
	seal     func(*task.ProviderExitRecord)
}

func (r traceRun) evaluate(t *testing.T) (task.Interpretation, string) {
	t.Helper()
	stdout := []byte(strings.Join(r.lines, "\n") + "\n")
	seal := alteredSeal(stdout, nil, func(seal *task.ProviderExitRecord) {
		seal.Predicate = r.revision.reference(Mode)
		if r.seal != nil {
			r.seal(seal)
		}
	})
	var answer bytes.Buffer
	result, err := (interactionInterpreter{mode: Mode, revision: r.revision}).Evaluate(predicate.Input{Seal: seal, ExpectedSession: r.expected}, &testEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("%s evaluate: %v", seal.Predicate.Version, err)
	}
	if result.Verdict != task.VerdictCommitted && answer.Len() != 0 {
		t.Fatalf("%s published an answer with verdict %s: %q", seal.Predicate.Version, result.Verdict, answer.String())
	}
	return result, answer.String()
}

func requireTrace(t *testing.T, r traceRun, verdict string) {
	t.Helper()
	result, answer := r.evaluate(t)
	if result.Verdict != verdict {
		t.Fatalf("%s verdict=%s want=%s diagnostics=%+v", r.revision.reference(Mode).Version, result.Verdict, verdict, result.Diagnostics)
	}
	if verdict == task.VerdictCommitted && answer != traceAnswer {
		t.Fatalf("%s answer=%q want=%q", r.revision.reference(Mode).Version, answer, traceAnswer)
	}
}

func TestCurrentRevisionIgnoresStructurallyValidUnidentifiedNotifications(t *testing.T) {
	standard := joinTrace(nativeSetupTrace(), standardTurnTrace())
	for _, method := range []string{
		"thread/status/changed", "thread/name/updated", "item/commandExecution/outputDelta",
		"turn/future", "serverRequest/future", "item/future/delta",
	} {
		noisy := joinTrace(handledSetupTrace(), []string{rootNotification(method, `,"turnId":"turn-1","state":"completed"`)}, standardTurnTrace())
		requireTrace(t, traceRun{revision: interactionCurrent, lines: noisy}, task.VerdictCommitted)
		requireTrace(t, traceRun{revision: interactionStrict, lines: noisy}, task.VerdictRejected)
	}
	requireTrace(t, traceRun{revision: interactionCurrent, lines: standard}, task.VerdictCommitted)
	requireTrace(t, traceRun{revision: interactionStrict, lines: standard}, task.VerdictRejected)
	core := joinTrace(handledSetupTrace(), coreTurnTrace())
	requireTrace(t, traceRun{revision: interactionStrict, lines: core}, task.VerdictCommitted)

	afterCompletion := append(append([]string{}, core...), rootNotification("thread/closed", ""))
	requireTrace(t, traceRun{revision: interactionCurrent, lines: afterCompletion}, task.VerdictCommitted)
	requireTrace(t, traceRun{revision: interactionStrict, lines: afterCompletion}, task.VerdictRejected)

	foreignThread := joinTrace(handledSetupTrace(), []string{`{"method":"thread/future","params":{"threadId":"other-thread"}}`}, standardTurnTrace())
	requireTrace(t, traceRun{revision: interactionCurrent, lines: foreignThread}, task.VerdictCommitted)
}

func TestUnrecognizedNotificationCannotCompleteATurn(t *testing.T) {
	withoutCompletion := joinTrace(nativeSetupTrace(), standardTurnTrace()[:len(standardTurnTrace())-1])
	for _, lookalike := range []string{
		rootNotification("turn/future", `,"turn":{"id":"turn-1","status":"completed"}`),
		rootNotification("turn/finished", `,"turn":{"id":"turn-1","status":"completed"}`),
		rootNotification("thread/closed", ""),
		`{"method":"unrelated/completed","params":{"state":"completed"}}`,
	} {
		lines := append(append([]string{}, withoutCompletion...), lookalike)
		result, _ := traceRun{revision: interactionCurrent, lines: lines}.evaluate(t)
		if result.Verdict != task.VerdictRejected || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "incomplete_output" {
			t.Fatalf("lookalike %s result=%+v", lookalike, result)
		}
	}
}

func TestKnownHandlersRemainStrict(t *testing.T) {
	complete := rootTurnCompleted("turn-1", `"status":"completed"`)
	prefix := joinTrace(handledSetupTrace(), coreTurnTrace()[:2])
	withTail := func(tail ...string) []string { return append(append([]string{}, prefix...), tail...) }
	for name, lines := range map[string][]string{
		"missing completion":           prefix,
		"failed status":                withTail(rootTurnCompleted("turn-1", `"status":"failed"`)),
		"cancelled status":             withTail(rootTurnCompleted("turn-1", `"status":"interrupted"`)),
		"completed with error":         withTail(rootTurnCompleted("turn-1", `"status":"completed","error":{"message":"failed"}`)),
		"duplicate completion":         withTail(complete, complete),
		"wrong turn completion":        withTail(rootTurnCompleted("turn-2", `"status":"completed"`)),
		"duplicate status field":       withTail(rootTurnCompleted("turn-1", `"status":"failed","status":"completed"`)),
		"other thread completion only": withTail(strings.Replace(complete, testThreadID, "other-thread", 1)),
		"fatal error before success": withTail(
			rootNotification("error", `,"turnId":"turn-1","willRetry":false,"error":{"message":"failed"}`), complete),
		"error for another turn": withTail(
			rootNotification("error", `,"turnId":"turn-9","willRetry":true,"error":{"message":"retry"}`), complete),
		"duplicate item id": withTail(finalMessage("item-1", traceAnswer), complete),
		"blank final": joinTrace(handledSetupTrace(), []string{
			finalMessage("item-1", "  \n"), complete,
		}),
		"commentary only": joinTrace(handledSetupTrace(), []string{
			rootNotification("item/completed", `,"turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","phase":"commentary","text":"progress"}`), complete,
		}),
		"no final message":        joinTrace(handledSetupTrace(), []string{complete}),
		"final from other thread": joinTrace(handledSetupTrace(), []string{strings.Replace(finalMessage("item-1", traceAnswer), testThreadID, "other-thread", 1), complete}),
		"duplicate method field": withTail(
			`{"method":"turn/future","method":"turn/completed","params":{"threadId":"` + testThreadID + `","turn":{"id":"turn-1","status":"completed"}}}`),
		"duplicate thread field in completion": withTail(
			`{"method":"turn/completed","params":{"threadId":"other","threadId":"` + testThreadID + `","turn":{"id":"turn-1","status":"completed"}}}`),
		"method with result": withTail(`{"method":"thread/future","result":{},"params":{}}`, complete),
		"method with error":  withTail(`{"method":"thread/future","error":{"code":1},"params":{}}`, complete),
		"empty method":       withTail(`{"method":"","params":{}}`, complete),
		"numeric method":     withTail(`{"method":7,"params":{}}`, complete),
	} {
		for _, revision := range recordCaptureRevisions {
			t.Run(fmt.Sprintf("%s/%s", name, revision.reference(Mode).Version), func(t *testing.T) {
				requireTrace(t, traceRun{revision: revision, lines: lines}, task.VerdictRejected)
			})
		}
	}
}

func TestRetryableErrorStillCompletesOnBothRecordRevisions(t *testing.T) {
	lines := joinTrace(handledSetupTrace(), coreTurnTrace()[:2], []string{
		rootNotification("error", `,"turnId":"turn-1","willRetry":true,"error":{"message":"retrying"}`),
		rootTurnCompleted("turn-1", `"status":"completed"`),
	})
	for _, revision := range recordCaptureRevisions {
		requireTrace(t, traceRun{revision: revision, lines: lines}, task.VerdictCommitted)
	}
}

func TestSealAndIdentityStillGateCommitmentOnCurrentRevision(t *testing.T) {
	lines := joinTrace(nativeSetupTrace(), standardTurnTrace())
	for name, alter := range map[string]func(*task.ProviderExitRecord){
		"seal error":  func(s *task.ProviderExitRecord) { s.Error = "interactive protocol fault" },
		"exit status": func(s *task.ProviderExitRecord) { s.ExitCode = 3 },
		"start fault": func(s *task.ProviderExitRecord) { s.InvocationState = task.InvocationStartFailed; s.Error = "start" },
	} {
		result, _ := traceRun{revision: interactionCurrent, lines: lines, seal: alter}.evaluate(t)
		if result.Verdict != task.VerdictRejected || !strings.Contains(result.Refusal, refusalProviderFailed) {
			t.Fatalf("%s result=%+v", name, result)
		}
	}
	requireTrace(t, traceRun{revision: interactionCurrent, lines: lines, expected: task.SessionExpectation{Required: true, ID: testThreadID}}, task.VerdictCommitted)
	other := "223e4567-e89b-12d3-a456-426614174000"
	result, _ := traceRun{revision: interactionCurrent, lines: lines, expected: task.SessionExpectation{Required: true, ID: other}}.evaluate(t)
	if result.Verdict != task.VerdictRejected {
		t.Fatalf("wrong expected session result=%+v", result)
	}
}
