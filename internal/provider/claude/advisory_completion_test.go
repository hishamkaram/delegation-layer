package claude

import (
	"bytes"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// nativeNegativeCase is one caller turn whose principal result is a complete
// native negative completion. The current advisory revision lets the live
// transport finish and leaves the verdict to its sealed predicate; the frozen
// interactive-v5 revision faulted the transport and replayed incomplete.
type nativeNegativeCase struct {
	name, principal string
	want, field     string
	// frozenReceiptFault marks evidence the frozen live protocol refuses on
	// receipt rather than at completion.
	frozenReceiptFault bool
}

func nativeNegativeCases() []nativeNegativeCase {
	canary := `"` + unsupportedReasonCanary + `"`
	principal := principalLine()
	return []nativeNegativeCase{
		{name: "unknown stop_reason", principal: mutateLine(principal, `"stop_reason":"end_turn"`, `"stop_reason":`+canary), want: refusalUnsupportedCompletion, field: "stop_reason"},
		{name: "unknown terminal_reason", principal: mutateLine(principal, `"terminal_reason":"completed"`, `"terminal_reason":`+canary), want: refusalUnsupportedCompletion, field: "terminal_reason", frozenReceiptFault: true},
		{name: "documented refusal", principal: mutateLine(principal, `"stop_reason":"end_turn"`, `"stop_reason":"refusal"`), want: refusalRefused},
		{name: "documented max_tokens", principal: mutateLine(principal, `"stop_reason":"end_turn"`, `"stop_reason":"max_tokens"`), want: refusalIncomplete},
		{name: "native provider failure", principal: `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"` + claudeTestUUID + `","errors":["failed"],"result_index":0}`, want: refusalProviderFailed + ": error_during_execution: failed"},
	}
}

// TestCurrentNativeNegativeCompletionSettlesTransport proves the advisory
// policy keeps native negative completion in its single owner: the live
// transport settles and completes cleanly, then print and sealed interactive
// replay reject with the same canonical class, a sanitized diagnostic, and no
// answer. Frozen interactive-v5 keeps its transport fault and incomplete class.
func TestCurrentNativeNegativeCompletionSettlesTransport(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, tc := range nativeNegativeCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				lines := []string{interactiveInitLine(mode), tc.principal}
				requireLiveCurrentSettles(t, mode, lines)
				sealed, sealedAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), wrapInteractiveSession(lines))
				requireUnsupportedVerdict(t, sealed, sealedAnswer, tc.want, tc.field)
				printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
				requireUnsupportedVerdict(t, printed, printedAnswer, tc.want, tc.field)
				requireFrozenInteractiveFault(t, mode, wrapInteractiveSession(lines), tc.frozenReceiptFault)
				frozen, frozenAnswer := evaluateInteractiveTrace(t, newScopedInteractionInterpreter(mode), wrapInteractiveSession(lines))
				if frozen.Verdict != task.VerdictRejected || frozen.Refusal != refusalIncomplete || frozenAnswer != "" {
					t.Fatalf("frozen interactive-v5 replay changed: %+v answer=%q", frozen, frozenAnswer)
				}
			})
		}
	}
}

// requireFrozenInteractiveFault requires the frozen interactive-v5 live
// protocol to keep refusing the trace on receipt or, after accepting every
// record, at completion.
func requireFrozenInteractiveFault(t *testing.T, mode string, trace []string, onReceipt bool) {
	t.Helper()
	p := liveInteractionWithPolicy(t, mode, scopedAttributedCompletion)
	if err := requireLiveError(p, trace); (err != nil) != onReceipt {
		t.Fatalf("frozen live receipt error=%v, want receipt fault %t", err, onReceipt)
	}
	if !onReceipt && !errors.Is(p.Complete(), task.ErrEvidenceFault) {
		t.Fatal("frozen live protocol completed a native negative caller turn")
	}
}

// TestCurrentNativeNegativeKeepsTransportAndSealPrecedence pairs an unknown
// principal stop_reason with every higher-priority fault: unsettled agents,
// questions, cycles, or batches keep the live transport open and replay
// incomplete; a wrong session faults on receipt; a syntax fault and a seal
// error or nonzero exit keep their own class. None publishes an answer.
func TestCurrentNativeNegativeKeepsTransportAndSealPrecedence(t *testing.T) {
	principal := nativeNegativeCases()[0].principal
	question := `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_question","input":{"questions":[{"question":"Choose","options":[{"label":"Blue"}]}]}}}`
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		for name, lines := range map[string][]string{
			"unsettled agent":    {initial, interactiveAgent, principal},
			"unsettled question": {initial, question, principal},
			"unsettled cycle":    {initial, principal, interactiveAssistant("neutral")},
			"unsettled batch":    {initial, principal, attributedResultLine("", notificationAttribution(1), 0, "0.2")},
		} {
			t.Run(mode+"/"+name, func(t *testing.T) { requireUnsettledNegativeStaysOpen(t, mode, lines) })
		}
		t.Run(mode+"/wrong session", func(t *testing.T) { requireWrongSessionPrecedesNegative(t, mode, []string{initial, principal}) })
		t.Run(mode+"/syntax fault", func(t *testing.T) {
			lines := []string{initial, principal, "{"}
			sealed, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), wrapInteractiveSession(lines))
			requireUnsupportedVerdict(t, sealed, answer, refusalIncomplete, "")
			printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
			requireUnsupportedVerdict(t, printed, printedAnswer, refusalMalformed, "")
		})
		for name, tc := range map[string]struct {
			seal func(*task.ProviderExitRecord)
			want string
		}{
			"nonzero exit": {func(seal *task.ProviderExitRecord) { seal.ExitCode = 3 }, refusalProviderFailed + ": exit_code=3"},
			"seal error":   {func(seal *task.ProviderExitRecord) { seal.Error = "sealed stream truncated" }, refusalProviderFailed + ": seal_error: sealed stream truncated"},
		} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				lines := []string{initial, principal}
				interactive, interactiveAnswer := evaluateWithSeal(t, newInteractionInterpreter(mode), traceBytes(wrapInteractiveSession(lines)), tc.seal)
				requireUnsupportedVerdict(t, interactive, interactiveAnswer, tc.want, "")
				printed, printedAnswer := evaluateWithSeal(t, NewInterpreter(mode), traceBytes(lines), tc.seal)
				requireUnsupportedVerdict(t, printed, printedAnswer, tc.want, "")
			})
		}
	}
}

// requireUnsettledNegativeStaysOpen requires an unsettled caller turn to keep
// the live input open and fault at completion, and its replay to be incomplete.
func requireUnsettledNegativeStaysOpen(t *testing.T, mode string, lines []string) {
	t.Helper()
	trace := wrapInteractiveSession(lines)
	p := liveInteractionWithPolicy(t, mode, advisoryScopedCompletion)
	for index, line := range trace {
		if update := receiveClaudeRecord(t, p, line); update.Done {
			t.Fatalf("live record %d closed an unsettled caller turn", index)
		}
	}
	if !errors.Is(p.Complete(), task.ErrEvidenceFault) {
		t.Fatal("live protocol completed an unsettled caller turn")
	}
	sealed, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), trace)
	requireUnsupportedVerdict(t, sealed, answer, refusalIncomplete, "")
}

// requireWrongSessionPrecedesNegative requires a session other than the
// expected one to fault the live transport on receipt and to keep its class
// on replay: incomplete for interactive and identity for print.
func requireWrongSessionPrecedesNegative(t *testing.T, mode string, lines []string) {
	t.Helper()
	other := task.SessionExpectation{Required: true, ID: "123e4567-e89b-12d3-a456-426614174001"}
	request := task.TaskRecord{RootID: claudeTestRootID, TaskID: claudeTestTaskID, Mode: mode}
	protocol, err := newClaudeInteractionWithCapture(request, other, nil, true, advisoryScopedCompletion)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong protocol")
	}
	if err = requireLiveError(p, wrapInteractiveSession(lines)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("live wrong session error=%v", err)
	}
	sealed, answer := evaluateExpected(t, newInteractionInterpreter(mode), traceBytes(wrapInteractiveSession(lines)), other)
	requireUnsupportedVerdict(t, sealed, answer, refusalIncomplete, "")
	printed, printedAnswer := evaluateExpected(t, NewInterpreter(mode), traceBytes(lines), other)
	requireUnsupportedVerdict(t, printed, printedAnswer, refusalIdentity, "")
}

func traceBytes(lines []string) []byte {
	var stdout bytes.Buffer
	for _, line := range lines {
		stdout.WriteString(line + "\n")
	}
	return stdout.Bytes()
}

func evaluateWithSeal(t *testing.T, v predicate.Interpreter, stdout []byte, mutate func(*task.ProviderExitRecord)) (task.Interpretation, string) {
	t.Helper()
	input := revisionInput(stdout, v.Reference())
	mutate(&input.Seal)
	var answer bytes.Buffer
	interpretation, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}
