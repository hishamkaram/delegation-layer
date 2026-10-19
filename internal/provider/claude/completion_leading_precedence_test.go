package claude

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// A failed leading acknowledgement always rejects the task. Its canonical
// refusal is reported only when no higher-priority fault applies: malformed
// evidence is reported as such, and an interactive stream that is not settled
// and idle at EOF stays incomplete.

type leadingFailureCase struct {
	name string
	// acknowledgement is the failed leading acknowledgement, result_index 0.
	acknowledgement string
	refusal         string
}

func leadingFailureCases() []leadingFailureCase {
	healthy := leadingAck(0, "0.05")
	return []leadingFailureCase{
		{"failed", leadingFailure(0, ""), refusalProviderFailed + ": error_during_execution: failed"},
		{"aborted", mutateLine(healthy, `"queued_turn_count":0`, `"queued_turn_count":0,"terminal_reason":"aborted_tools"`), refusalProviderFailed + ": terminal_reason=aborted_tools"},
		{"queued", mutateLine(healthy, `"queued_turn_count":0`, `"queued_turn_count":2`), refusalIncomplete},
		{"refused", mutateLine(healthy, `"stop_reason":"end_turn"`, `"stop_reason":"refusal"`), refusalRefused},
	}
}

// invalidScopeMessage is a record the scoped policy reports as malformed,
// before or after the caller's result.
func invalidScopeMessage() string {
	return scopeMessage("assistant", `,"parent_tool_use_id":""`, scopeTextBlock)
}

func requireRejection(t *testing.T, label string, interpretation task.Interpretation, answer, refusal string) {
	t.Helper()
	if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != refusal || answer != "" {
		t.Fatalf("%s interpretation=%+v answer=%q want refusal %q", label, interpretation, answer, refusal)
	}
}

func interactiveWrap(lines []string) []string {
	return append(append([]string{interactiveRunning}, lines...), interactiveIdle)
}

func TestLeadingFailedAcknowledgementKeepsCanonicalRefusalWhenCallerSucceeds(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		root := scopeMessage("assistant", scopeRootParent, scopeTextBlock)
		for _, tc := range leadingFailureCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				settled := []string{leadingStopped(1), initial, tc.acknowledgement, initial, root, leadingCaller(1, "0.35")}
				printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), settled)
				requireRejection(t, "print", printed, printedAnswer, tc.refusal)
				interactive, interactiveAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), interactiveWrap(settled))
				requireRejection(t, "interactive settled", interactive, interactiveAnswer, tc.refusal)

				// The same trace without the final idle is unfinished at EOF.
				unfinished := append([]string{interactiveRunning}, settled...)
				interactive, interactiveAnswer = evaluateInteractiveTrace(t, newInteractionInterpreter(mode), unfinished)
				requireRejection(t, "interactive without idle", interactive, interactiveAnswer, refusalIncomplete)
			})
		}
	}
}

func TestLeadingFailedAcknowledgementKeepsCanonicalRefusalWhenCallerNeverArrives(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		for _, tc := range leadingFailureCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				lines := []string{leadingStopped(1), initial, tc.acknowledgement}
				printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
				requireRejection(t, "print", printed, printedAnswer, tc.refusal)
				interactive, interactiveAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), interactiveWrap(lines))
				requireRejection(t, "interactive", interactive, interactiveAnswer, tc.refusal)
			})
		}
	}
}

func TestLeadingFailedAcknowledgementYieldsToMalformedEvidence(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		root := scopeMessage("assistant", scopeRootParent, scopeTextBlock)
		for _, tc := range leadingFailureCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				withCaller := []string{leadingStopped(1), initial, tc.acknowledgement, initial, root, leadingCaller(1, "0.35"), invalidScopeMessage()}
				printed, printedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), withCaller)
				requireRejection(t, "print with caller", printed, printedAnswer, refusalMalformed)
				// Interactive predicates report a malformed stream as incomplete.
				interactive, interactiveAnswer := evaluateInteractiveTrace(t, newInteractionInterpreter(mode), interactiveWrap(withCaller))
				requireRejection(t, "interactive with caller", interactive, interactiveAnswer, refusalIncomplete)

				withoutCaller := []string{leadingStopped(1), initial, tc.acknowledgement, invalidScopeMessage()}
				printed, printedAnswer = evaluateWriteTrace(t, NewInterpreter(mode), withoutCaller)
				requireRejection(t, "print without caller", printed, printedAnswer, refusalMalformed)
			})
		}
	}
}

// TestFrozenRevisionsKeepFailedAcknowledgementInterpretation pins the released
// refusal classes for a failed acknowledgement followed by a successful caller:
// only the current scoped revisions report the acknowledgement's refusal.
func TestFrozenRevisionsKeepFailedAcknowledgementInterpretation(t *testing.T) {
	frozenWrite, err := retainedRegistry(t).Resolve(unscopedReference(WorkspaceWriteMode))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		initial := interactiveInitLine(mode)
		root := scopeMessage("assistant", scopeRootParent, scopeTextBlock)
		lines := []string{leadingStopped(1), initial, leadingFailure(0, ""), initial, root, leadingCaller(1, "0.35")}
		preAttribution, answer := evaluateWriteTrace(t, newPreAttributionInterpreter(mode), lines)
		requireRejection(t, mode+" pre-attribution print", preAttribution, answer, refusalMalformed)
		if mode == WorkspaceWriteMode {
			unscoped, unscopedAnswer := evaluateWriteTrace(t, frozenWrite, lines)
			requireRejection(t, "unscoped native-permissions-v5", unscoped, unscopedAnswer, refusalMalformed)
		}
		unscopedInteractive, unscopedAnswer := evaluateInteractiveTrace(t, newUnscopedInteractionInterpreter(mode), interactiveWrap(lines))
		requireRejection(t, mode+" interactive-v4", unscopedInteractive, unscopedAnswer, refusalIncomplete)
		preInteractive, preAnswer := evaluateInteractiveTrace(t, newPreAttributionInteractionInterpreter(mode), interactiveWrap(lines))
		requireRejection(t, mode+" interactive-v3", preInteractive, preAnswer, refusalIncomplete)
	}
}
