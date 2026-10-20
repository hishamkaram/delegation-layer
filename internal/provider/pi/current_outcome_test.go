package pi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var piOutcomeModes = []string{ModeReadOnly, ModeWorkspaceWrite}

type piOutcomeResult struct {
	interpretation task.Interpretation
	answer         string
}

// evaluatePiOutcome runs the fresh-default interpreter for mode over stdout,
// binding the seal to that interpreter's own reference like a real collection.
func evaluatePiOutcome(t *testing.T, mode string, stdout []byte) piOutcomeResult {
	t.Helper()
	var answer bytes.Buffer
	interpreter := NewInterpreter(mode)
	interpretation, err := interpreter.Evaluate(
		predicate.Input{Seal: validSeal(interpreter.Reference(), stdout, nil)}, evidence{stdout: stdout}, &answer,
	)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	return piOutcomeResult{interpretation: interpretation, answer: answer.String()}
}

func requirePiOutcomeCommitted(t *testing.T, result piOutcomeResult, answer string) {
	t.Helper()
	if result.interpretation.Verdict != task.VerdictCommitted || result.interpretation.Refusal != "" || result.answer != answer {
		t.Fatalf("result=%+v answer=%q want committed %q", result.interpretation, result.answer, answer)
	}
}

func requirePiOutcomeRejected(t *testing.T, result piOutcomeResult, refusal string) {
	t.Helper()
	if result.interpretation.Verdict != task.VerdictRejected || result.interpretation.Refusal != refusal || result.answer != "" {
		t.Fatalf("result=%+v answer=%q want rejected %q with no answer", result.interpretation, result.answer, refusal)
	}
}

func withPiLines(stream []byte, lines ...string) []byte {
	return []byte(string(stream) + "\n" + strings.Join(lines, "\n"))
}

func insertPiLineAfter(t *testing.T, stream []byte, marker, line string) []byte {
	t.Helper()
	lines := strings.Split(string(stream), "\n")
	for index, candidate := range lines {
		if strings.Contains(candidate, marker) {
			return []byte(strings.Join(append(lines[:index+1], append([]string{line}, lines[index+1:]...)...), "\n"))
		}
	}
	t.Fatalf("marker %q not found", marker)
	return nil
}

func TestPiCurrentSettledAbortedIsNotSuccess(t *testing.T) {
	complete := piStream(testUUID, "otherwise complete", true)
	for _, mode := range piOutcomeModes {
		t.Run(mode, func(t *testing.T) {
			requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(complete, `{"type":"agent_settled","aborted":true}`)), refusalProviderFailed)
			requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, withPiLines(complete, `{"type":"agent_settled","aborted":false}`)), "otherwise complete")
			requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, withPiLines(complete, `{"type":"agent_settled"}`)), "otherwise complete")
			requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, complete), "otherwise complete")
		})
	}
}

func TestPiCurrentSettledAbortedMustBeAnUnambiguousBoolean(t *testing.T) {
	complete := piStream(testUUID, "otherwise complete", true)
	for _, settled := range []string{
		`{"type":"agent_settled","aborted":null}`,
		`{"type":"agent_settled","aborted":"true"}`,
		`{"type":"agent_settled","aborted":1}`,
		`{"type":"agent_settled","aborted":[true]}`,
		`{"type":"agent_settled","aborted":{}}`,
		`{"type":"agent_settled","aborted":false,"aborted":true}`,
		`{"type":"agent_settled","aborted":true,"aborted":false}`,
		`{"type":"agent_settled","aborted":false,"Aborted":true}`,
	} {
		t.Run(settled, func(t *testing.T) {
			for _, mode := range piOutcomeModes {
				requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(complete, settled)), refusalMalformed)
			}
		})
	}
}

func TestPiCurrentSettledAbortedKeepsFailureAndLifecyclePrecedence(t *testing.T) {
	aborted := `{"type":"agent_settled","aborted":true}`
	incomplete := withPiLines(piStream(testUUID, "never ended", false), aborted)
	identity := evaluatePiOutcomeWithExpectation(t, withPiLines(piStream(testUUID, "answer", true), aborted),
		task.SessionExpectation{Required: true, ID: "123e4567-e89b-12d3-a456-426614174001"})
	for _, mode := range piOutcomeModes {
		requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, incomplete), refusalMalformed)
		requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(piStream(testUUID, "answer", true), aborted, aborted)), refusalMalformed)
	}
	requirePiOutcomeRejected(t, identity, refusalIdentity)
	blank := evaluatePiOutcome(t, ModeReadOnly, withPiLines(piStream(testUUID, "   ", true), aborted))
	requirePiOutcomeRejected(t, blank, refusalProviderFailed)
}

func evaluatePiOutcomeWithExpectation(t *testing.T, stdout []byte, expected task.SessionExpectation) piOutcomeResult {
	t.Helper()
	var answer bytes.Buffer
	interpreter := NewInterpreter(ModeReadOnly)
	interpretation, err := interpreter.Evaluate(
		predicate.Input{Seal: validSeal(interpreter.Reference(), stdout, nil), ExpectedSession: expected}, evidence{stdout: stdout}, &answer,
	)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	return piOutcomeResult{interpretation: interpretation, answer: answer.String()}
}

func TestPiCurrentRetryEndFailureIsNotSuccess(t *testing.T) {
	retried := piNativeRetryStream(testUUID, "after retry", true)
	failedEnd := strings.Replace(string(retried), `"auto_retry_end","attempt":1,"success":true`, `"auto_retry_end","attempt":1,"success":false,"finalError":"retry failed"`, 1)
	absent := strings.Replace(string(retried), `"auto_retry_end","attempt":1,"success":true`, `"auto_retry_end","attempt":1`, 1)
	for _, mode := range piOutcomeModes {
		t.Run(mode, func(t *testing.T) {
			requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, retried), "after retry")
			requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, []byte(absent)), "after retry")
			requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, []byte(failedEnd)), refusalProviderFailed)
		})
	}
}

func TestPiCurrentRetryEndFailureAfterAgentEndIsNotSuccess(t *testing.T) {
	// A retry sequence that reports final failure after an agent_end whose
	// last message looked complete must not be published as the answer.
	complete := piStream(testUUID, "looks complete", true)
	stdout := withPiLines(complete, `{"type":"auto_retry_start","attempt":1}`)
	retryFailed := withPiLines(stdout, `{"type":"auto_retry_end","success":false,"finalError":"gave up"}`)
	for _, mode := range piOutcomeModes {
		// Pending retry without another cycle stays incomplete: the existing
		// lifecycle precedence is unchanged by the new failure signal.
		requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, retryFailed), refusalIncomplete)
	}
}

func TestPiCurrentRetryEndSuccessMustBeAnUnambiguousBoolean(t *testing.T) {
	retried := string(piNativeRetryStream(testUUID, "after retry", true))
	for _, replacement := range []string{
		`"success":null`,
		`"success":"true"`,
		`"success":0`,
		`"success":{}`,
		`"success":true,"success":false`,
		`"success":true,"Success":false`,
	} {
		t.Run(replacement, func(t *testing.T) {
			stdout := []byte(strings.Replace(retried, `"success":true`, replacement, 1))
			for _, mode := range piOutcomeModes {
				requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, stdout), refusalMalformed)
			}
		})
	}
}

func TestPiCurrentRetryFailureDoesNotPoisonLaterIndependentCycle(t *testing.T) {
	// The existing reducer scopes failure to the cycle: a later complete cycle
	// recovers, exactly as a failed cycle followed by a successful retry does.
	failed := `{"role":"assistant","content":[{"type":"thinking"}],"stopReason":"error","errorMessage":"provider error"}`
	first := withPiLines([]byte(strings.Join([]string{
		sessionLine(testUUID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_end","message":` + failed + `}`,
		`{"type":"turn_end","message":` + failed + `,"toolResults":[]}`,
		`{"type":"agent_end","messages":[` + failed + `]}`,
	}, "\n")), `{"type":"auto_retry_end","success":false,"finalError":"gave up"}`)
	second := strings.Split(string(piStream(testUUID, "independent final", true)), "\n")[1:]
	stdout := withPiLines(first, second...)
	for _, mode := range piOutcomeModes {
		requirePiOutcomeCommitted(t, evaluatePiOutcome(t, mode, stdout), "independent final")
	}
}

func TestPiCurrentObservationalEventsDoNotChangeAnswerOrState(t *testing.T) {
	observations := []string{
		`{"type":"session_info_changed","name":"renamed session"}`,
		`{"type":"session_info_changed"}`,
		`{"type":"session_info_changed","name":"a","extra":{"nested":[1,2,{"x":null}],"dup":1,"dup":2}}`,
		`{"type":"thinking_level_changed","level":"high"}`,
	}
	base := piStream(testUUID, "exact answer", true)
	for _, observation := range observations {
		for _, mode := range piOutcomeModes {
			t.Run(mode+"/"+observation, func(t *testing.T) {
				afterSession := insertPiLineAfter(t, base, `"type":"session"`, observation)
				midCycle := insertPiLineAfter(t, base, `"type":"turn_start"`, observation)
				afterEnd := withPiLines(base, observation)
				afterSettled := withPiLines(base, `{"type":"agent_settled","aborted":false}`, observation)
				for name, stdout := range map[string][]byte{"after session": afterSession, "mid cycle": midCycle, "after agent_end": afterEnd, "after settled": afterSettled} {
					result := evaluatePiOutcome(t, mode, stdout)
					if result.interpretation.Verdict != task.VerdictCommitted || result.answer != "exact answer" {
						t.Fatalf("%s: result=%+v answer=%q", name, result.interpretation, result.answer)
					}
				}
			})
		}
	}
}

func TestPiCurrentObservationalEventsDoNotRescueAbortedOrBlankEvidence(t *testing.T) {
	observation := `{"type":"session_info_changed","name":"renamed"}`
	aborted := withPiLines(piStream(testUUID, "answer", true), `{"type":"agent_settled","aborted":true}`)
	for _, mode := range piOutcomeModes {
		requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(aborted, observation)), refusalProviderFailed)
		requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(piStream(testUUID, "   ", true), observation)), refusalEmpty)
	}
}

func TestPiCurrentObservationalEventsRequireValidSyntaxAndObjectShape(t *testing.T) {
	base := piStream(testUUID, "answer", true)
	for _, line := range []string{
		`{"type":"session_info_changed","name":`,
		`["session_info_changed"]`,
		`{"type":"session_info_changed","name":"a"} trailing`,
	} {
		t.Run(line, func(t *testing.T) {
			for _, mode := range piOutcomeModes {
				requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(base, line)), refusalMalformed)
			}
		})
	}
}

func TestPiCurrentSettlementStillRejectsWorkAndAuthorityEvents(t *testing.T) {
	base := withPiLines(piStream(testUUID, "answer", true), `{"type":"agent_settled","aborted":false}`)
	message := `{"role":"assistant","content":[{"type":"text","text":"late"}],"stopReason":"stop"}`
	for _, late := range []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"turn_end","message":` + message + `,"toolResults":[]}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_end","message":` + message + `}`,
		`{"type":"agent_end","messages":[` + message + `]}`,
		`{"type":"agent_settled","aborted":false}`,
		`{"type":"auto_retry_start","attempt":1}`,
		`{"type":"auto_retry_end","success":true}`,
		`{"type":"error","message":"late failure"}`,
		sessionLine(testUUID),
		`{"type":"session_compact"}`,
		`{"type":"agent_interrupted"}`,
		`{"type":"telemetry"}`,
	} {
		t.Run(late, func(t *testing.T) {
			for _, mode := range piOutcomeModes {
				requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(base, late)), refusalMalformed)
			}
		})
	}
}

func TestPiCurrentUnknownLifecycleStillRejectsBeforeSettlement(t *testing.T) {
	base := piStream(testUUID, "answer", true)
	for _, unknown := range []string{`{"type":"session_compact"}`, `{"type":"agent_interrupted"}`, `{"type":"turn_paused"}`} {
		for _, mode := range piOutcomeModes {
			requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, withPiLines(base, unknown)), refusalMalformed)
			requirePiOutcomeRejected(t, evaluatePiOutcome(t, mode, insertPiLineAfter(t, base, `"type":"turn_start"`, unknown)), refusalMalformed)
		}
	}
}
