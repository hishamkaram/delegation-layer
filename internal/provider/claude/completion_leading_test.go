package claude

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Sanitized native-shaped fixtures for a resumed session. Claude Code can
// answer old background completions with empty zero-turn results that precede
// the caller's own result: each carries origin task-notification and the next
// result_index, and only the final result answers the caller. The fixtures keep
// that shape and nothing else of a real trace.

func leadingStopped(n int) string {
	return fmt.Sprintf(`{"type":"system","subtype":"task_notification","task_id":"bg-old-%d","status":"stopped"}`, n)
}

// leadingAck is a leading acknowledgement as observed: empty result, zero turns,
// no terminal_reason, queued_turn_count 0.
func leadingAck(index int, cost string) string {
	line := attributedResultLine("", notificationAttribution(index), 0, cost)
	return strings.Replace(line, `"terminal_reason":"completed",`, "", 1)
}

func leadingCaller(index int, cost string) string {
	return attributedResultLine(tracePrincipalText, fmt.Sprintf(`,"result_index":%d`, index), 1, cost)
}

func leadingFailure(index int, fields string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":%q,"errors":["failed"],"num_turns":0%s,"result_index":%d,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-%d"}}`, claudeTestUUID, fields, index, index)
}

func mutateLine(line, old, replacement string) string {
	if !strings.Contains(line, old) {
		panic("fixture mutation does not apply: " + old)
	}
	return strings.Replace(line, old, replacement, 1)
}

type leadingCase struct {
	name   string
	lines  []string
	commit bool
	// refusal, when set, is the exact canonical class of the rejection.
	refusal string
}

func leadingCases(mode string) []leadingCase {
	initial := interactiveInitLine(mode)
	root := scopeMessage("assistant", scopeRootParent, scopeTextBlock)
	nested := scopeMessage("assistant", scopeChildParent, scopeChildToolUse)
	ack0, ack1 := leadingAck(0, "0.05"), leadingAck(1, "0.06")
	caller1, caller2 := leadingCaller(1, "0.35"), leadingCaller(2, "0.36")
	notices := []string{leadingStopped(1), leadingStopped(2)}
	prefix := func(lines ...string) []string { return append(append([]string{}, notices...), lines...) }
	failed := leadingFailure(0, "")
	cases := []leadingCase{
		{"native-shaped leading acknowledgement", prefix(initial, ack0, initial, root, caller1), true, ""},
		{"acknowledgement with completed terminal reason", prefix(initial, attributedResultLine("", notificationAttribution(0), 0, "0.05"), initial, root, caller1), true, ""},
		{"two consecutive acknowledgements", prefix(initial, ack0, ack1, initial, root, caller2), true, ""},
		{"acknowledgement without repeated init", prefix(initial, ack0, root, caller1), true, ""},
		{"init between acknowledgements", prefix(initial, ack0, initial, ack1, initial, root, caller2), true, ""},
		{"later notification after the caller", prefix(initial, ack0, initial, root, caller1, initial, root, attributedResultLine("neutral", notificationAttribution(2), 1, "0.4")), true, ""},
		{"nested messages around an acknowledgement", prefix(initial, ack0, nested, initial, nested, root, caller1), true, ""},
		{"acknowledgements only", prefix(initial, ack0), false, refusalIncomplete},
		{"acknowledgement and repeated init only", prefix(initial, ack0, initial), false, refusalIncomplete},
		{"acknowledgement before init", prefix(ack0, initial, root, caller1), false, refusalMalformed},
		{"missing counter", prefix(initial, mutateLine(ack0, `,"result_index":0`, ""), initial, root, caller1), false, refusalMalformed},
		{"mixed counters", prefix(initial, ack0, mutateLine(ack1, `,"result_index":1`, ""), initial, root, caller2), false, refusalMalformed},
		{"gapped counters", prefix(initial, ack0, leadingAck(2, "0.06"), initial, root, leadingCaller(3, "0.35")), false, refusalMalformed},
		{"counter not starting at zero", prefix(initial, leadingAck(1, "0.05"), initial, root, caller2), false, refusalMalformed},
		{"duplicate counter", prefix(initial, ack0, leadingAck(0, "0.06"), initial, root, caller1), false, refusalMalformed},
		{"caller repeats the acknowledgement counter", prefix(initial, ack0, initial, root, leadingCaller(0, "0.35")), false, refusalMalformed},
		{"caller without a counter", prefix(initial, ack0, initial, root, attributedResultLine(tracePrincipalText, "", 1, "0.35")), false, refusalMalformed},
		{"caller counter gap", prefix(initial, ack0, initial, root, leadingCaller(2, "0.35")), false, refusalMalformed},
		{"caller from a notification", prefix(initial, ack0, initial, root, attributedResultLine(tracePrincipalText, notificationAttribution(1), 1, "0.35")), false, refusalMalformed},
		{"nonempty acknowledgement", prefix(initial, mutateLine(ack0, `"result":""`, `"result":"x"`), initial, root, caller1), false, refusalMalformed},
		{"whitespace acknowledgement", prefix(initial, mutateLine(ack0, `"result":""`, `"result":" "`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement without result", prefix(initial, mutateLine(ack0, `"result":"",`, ""), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with mistyped result", prefix(initial, mutateLine(ack0, `"result":""`, `"result":7`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement without num_turns", prefix(initial, mutateLine(ack0, `,"num_turns":0`, ""), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with turns", prefix(initial, mutateLine(ack0, `"num_turns":0`, `"num_turns":1`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with mistyped num_turns", prefix(initial, mutateLine(ack0, `"num_turns":0`, `"num_turns":"0"`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with subkind", prefix(initial, mutateLine(ack0, `"kind":"task-notification",`, `"kind":"task-notification","subkind":"scheduled-trigger",`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with peer origin", prefix(initial, mutateLine(ack0, `"kind":"task-notification"`, `"kind":"peer"`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement with undocumented terminal reason", prefix(initial, mutateLine(ack0, `"queued_turn_count":0`, `"queued_turn_count":0,"terminal_reason":"paused"`), initial, root, caller1), false, refusalMalformed},
		{"acknowledgement from another session", prefix(initial, mutateLine(ack0, claudeTestUUID, "123e4567-e89b-12d3-a456-426614174001"), initial, root, caller1), false, refusalMalformed},
		{"root message before acknowledgement", prefix(initial, root, ack0, initial, caller1), false, refusalMalformed},
		{"duplicate init before acknowledgement", prefix(initial, initial, ack0, root, caller1), false, refusalMalformed},
		{"two inits for one acknowledgement", prefix(initial, ack0, initial, initial, root, caller1), false, refusalMalformed},
		{"init after a root message", prefix(initial, ack0, initial, root, initial, caller1), false, refusalMalformed},
		{"init without an acknowledgement", prefix(initial, root, initial, caller1), false, refusalMalformed},
		{"changed init cwd", prefix(initial, ack0, mutateLine(initial, `"cwd":"/workspace"`, `"cwd":"/elsewhere"`), root, caller1), false, refusalMalformed},
		{"changed init session", prefix(initial, ack0, mutateLine(initial, claudeTestUUID, "123e4567-e89b-12d3-a456-426614174001"), root, caller1), false, refusalMalformed},
		{"failed acknowledgement with a later caller", prefix(initial, failed, initial, root, caller1), false, refusalProviderFailed + ": error_during_execution: failed"},
		{"failed acknowledgement only", prefix(initial, failed), false, refusalProviderFailed + ": error_during_execution: failed"},
		{"aborted acknowledgement with a later caller", prefix(initial, mutateLine(ack0, `"queued_turn_count":0`, `"queued_turn_count":0,"terminal_reason":"aborted_tools"`), initial, root, caller1), false, refusalProviderFailed + ": terminal_reason=aborted_tools"},
		{"queued acknowledgement with a later caller", prefix(initial, mutateLine(ack0, `"queued_turn_count":0`, `"queued_turn_count":2`), initial, root, caller1), false, refusalIncomplete},
		{"queued acknowledgement only", prefix(initial, mutateLine(ack0, `"queued_turn_count":0`, `"queued_turn_count":2`)), false, refusalIncomplete},
		{"refused acknowledgement with a later caller", prefix(initial, mutateLine(ack0, `"stop_reason":"end_turn"`, `"stop_reason":"refusal"`), initial, root, caller1), false, refusalRefused},
		{"failed acknowledgement with a bad counter", prefix(initial, leadingFailure(1, ""), initial, root, caller1), false, refusalMalformed},
	}
	return cases
}

// requireLeadingVerdict checks a verdict. Interactive predicates report every
// malformed or conflicting stream as incomplete output, as they always have;
// concrete provider refusal classes are exact on both predicates.
func requireLeadingVerdict(t *testing.T, tc leadingCase, interactive bool, interpretation task.Interpretation, answer string) {
	t.Helper()
	if interactive && tc.refusal == refusalMalformed {
		tc.refusal = refusalIncomplete
	}
	if tc.commit {
		if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
			t.Fatalf("interpretation=%+v answer=%q", interpretation, answer)
		}
		return
	}
	if interpretation.Verdict != task.VerdictRejected || answer != "" || (tc.refusal != "" && interpretation.Refusal != tc.refusal) {
		t.Fatalf("interpretation=%+v answer=%q want refusal %q", interpretation, answer, tc.refusal)
	}
}

func TestLeadingAcknowledgementsPrintCompletion(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, tc := range leadingCases(mode) {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				interpretation, answer := evaluateWriteTrace(t, NewInterpreter(mode), tc.lines)
				requireLeadingVerdict(t, tc, false, interpretation, answer)
			})
		}
	}
}

func TestLeadingAcknowledgementsInteractiveSealedCompletion(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, tc := range leadingCases(mode) {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				lines := append(append([]string{interactiveRunning}, tc.lines...), interactiveIdle)
				interpretation, answer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), lines)
				requireLeadingVerdict(t, tc, true, interpretation, answer)
			})
		}
	}
}

// TestLeadingAcknowledgementsKeepPrincipalUsageAndLatestCost proves an
// acknowledgement never contributes main-agent usage, turns, or a cost baseline:
// the caller's result owns them and a later notification replaces only the
// cumulative whole-tree cost.
func TestLeadingAcknowledgementsKeepPrincipalUsageAndLatestCost(t *testing.T) {
	wantCosts := map[string]string{"native-shaped leading acknowledgement": "0.35", "later notification after the caller": "0.4", "two consecutive acknowledgements": "0.36"}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		for _, tc := range leadingCases(mode) {
			wantCost, ok := wantCosts[tc.name]
			if !ok {
				continue
			}
			printed, _ := evaluateWriteTrace(t, NewInterpreter(mode), tc.lines)
			interactive, _ := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), append(append([]string{interactiveRunning}, tc.lines...), interactiveIdle))
			requireLeadingUsage(t, mode+"/print/"+tc.name, printed, wantCost)
			requireLeadingUsage(t, mode+"/interactive/"+tc.name, interactive, wantCost)
		}
	}
}

func requireLeadingUsage(t *testing.T, label string, interpretation task.Interpretation, wantCost string) {
	t.Helper()
	if interpretation.Verdict != task.VerdictCommitted || len(interpretation.Usage) != 2 {
		t.Fatalf("%s interpretation=%+v", label, interpretation)
	}
	main, whole := interpretation.Usage[0], interpretation.Usage[1]
	assertClaudeInt64(t, label+" principal num_turns", main.NumTurns, 1)
	assertClaudeInt64(t, label+" principal input tokens", main.InputTokens, 12)
	if main.Scope != task.UsageScopeMainAgent || whole.EstimatedCostUSD == nil || *whole.EstimatedCostUSD != wantCost {
		t.Fatalf("%s usage=%+v whole=%+v want cost %s", label, main, whole, wantCost)
	}
}

func TestLeadingAcknowledgementsLiveInteractionDoesNotCloseOrFail(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode, func(t *testing.T) {
			p := liveAttributedInteractionIn(t, mode)
			initial := interactiveInitLine(mode)
			for _, line := range []string{leadingStopped(1), interactiveRunning, initial, leadingAck(0, "0.05")} {
				if update := receiveClaudeRecord(t, p, line); update.Done {
					t.Fatalf("closed input on %s", line)
				}
			}
			if update := receiveClaudeRecord(t, p, interactiveIdle); update.Done || p.failed || p.resultSeen {
				t.Fatalf("acknowledgement and idle closed or failed the interaction: done=%v failed=%v result=%v", update.Done, p.failed, p.resultSeen)
			}
			if p.Complete() == nil {
				t.Fatal("acknowledgement-only end of stream completed")
			}
			requireLiveCallerSettles(t, p, initial)
		})
	}
}

// requireLiveCallerSettles feeds the caller's turn after an acknowledgement and
// requires closure only once the session is idle again.
func requireLiveCallerSettles(t *testing.T, p *claudeInteraction, initial string) {
	t.Helper()
	receiveClaudeRecord(t, p, interactiveRunning)
	receiveClaudeRecord(t, p, initial)
	receiveClaudeRecord(t, p, interactiveAssistant("neutral"))
	if update := receiveClaudeRecord(t, p, leadingCaller(1, "0.35")); update.Done {
		t.Fatal("input closed before the session went idle")
	}
	if update := receiveClaudeRecord(t, p, interactiveIdle); !update.Done || p.failed || p.Complete() != nil {
		t.Fatalf("caller result did not settle: done=%v failed=%v", update.Done, p.failed)
	}
}

func TestLeadingAcknowledgementsLiveInteractionRejectsUnsupportedInit(t *testing.T) {
	for name, lines := range map[string][]string{
		"duplicate before acknowledgement": {interactiveInitLine(Mode)},
		"second init for one":              {interactiveInitLine(Mode), leadingAck(0, "0.05"), interactiveInitLine(Mode), interactiveInitLine(Mode)},
		"after a root message":             {interactiveInitLine(Mode), leadingAck(0, "0.05"), interactiveInitLine(Mode), interactiveAssistant("neutral"), interactiveInitLine(Mode)},
	} {
		t.Run(name, func(t *testing.T) {
			p := liveAttributedInteractionIn(t, Mode)
			var err error
			for _, line := range append(lines, interactiveInitLine(Mode)) {
				if _, err = p.ReceiveRecord(recordValue(line)); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("unsupported repeated init was accepted")
			}
		})
	}
}

// TestFrozenRevisionsKeepRejectingLeadingAcknowledgements pins the released
// interpretations of the same native-shaped prefix.
func TestFrozenRevisionsKeepRejectingLeadingAcknowledgements(t *testing.T) {
	registry := retainedRegistry(t)
	frozen, err := registry.Resolve(unscopedReference(WorkspaceWriteMode))
	if err != nil {
		t.Fatal(err)
	}
	native := leadingCases(WorkspaceWriteMode)[0].lines
	if got, answer := evaluateWriteTrace(t, frozen, native); got.Verdict != task.VerdictRejected || answer != "" {
		t.Fatalf("retained unscoped write revision changed: %+v answer=%q", got, answer)
	}
	if got, answer := evaluateWriteTrace(t, newPreAttributionInterpreter(WorkspaceWriteMode), native); got.Verdict != task.VerdictRejected || answer != "" {
		t.Fatalf("pre-attribution revision changed: %+v answer=%q", got, answer)
	}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := append(append([]string{interactiveRunning}, leadingCases(mode)[0].lines...), interactiveIdle)
		if got, answer := evaluateInteractiveTrace(t, newUnscopedInteractionInterpreter(mode), lines); got.Verdict != task.VerdictRejected || answer != "" {
			t.Fatalf("%s interactive-v4 changed: %+v answer=%q", mode, got, answer)
		}
		if got, answer := evaluateInteractiveTrace(t, newPreAttributionInteractionInterpreter(mode), lines); got.Verdict != task.VerdictRejected || answer != "" {
			t.Fatalf("%s interactive-v3 changed: %+v answer=%q", mode, got, answer)
		}
	}
}
