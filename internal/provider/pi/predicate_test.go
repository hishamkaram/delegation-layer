package pi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type evidence struct {
	stdout []byte
	stderr []byte
}

func (e evidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if consume == nil {
		return errors.New("nil evidence callback")
	}
	data := e.stderr
	if stream == predicate.Stdout {
		data = e.stdout
	}
	return consume(bytes.NewReader(data))
}

func piStream(sessionID, answer string, includeTerminal bool) []byte {
	message := fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q}],"stopReason":"stop"}`, answer)
	lines := []string{
		sessionLine(sessionID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","usage":{"input_tokens":3,"output_tokens":2},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"ignored"}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, message),
	}
	if includeTerminal {
		lines = append(lines,
			fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, message),
			fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, message),
		)
	}
	return []byte(strings.Join(lines, "\n"))
}

func piToolStream(sessionID, answer string) []byte {
	toolMessage := `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{}}],"stopReason":"toolUse"}`
	finalMessage := fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q}],"stopReason":"stop"}`, answer)
	return []byte(strings.Join([]string{
		sessionLine(sessionID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, toolMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[{}]}`, toolMessage),
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, finalMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, finalMessage),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s,%s]}`, toolMessage, finalMessage),
	}, "\n"))
}

func validSeal(ref task.PredicateRef, stdout, stderr []byte) task.ProviderExitRecord {
	manifest := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: int64(len(stderr)), SHA256: task.ComputeSHA256(stderr)},
		{Path: "raw/stdout", Size: int64(len(stdout)), SHA256: task.ComputeSHA256(stdout)},
	}
	encoded, err := task.MarshalCanonical(manifest)
	if err != nil {
		panic(err)
	}
	return task.ProviderExitRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          testRootID,
		TaskID:          testTaskID,
		SpecSHA256:      strings.Repeat("a", 64),
		MetaSHA256:      strings.Repeat("b", 64),
		InvocationState: task.InvocationStarted,
		ExitCode:        0,
		Predicate:       ref,
		RawManifest:     manifest,
		ManifestSHA256:  task.ComputeSHA256(encoded),
		ClosedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func TestPiInterpreterCommitsValidatedJSONStream(t *testing.T) {
	stdout := piStream(testUUID, "answer from Pi", true)
	ref := ReferenceForMode(ModeReadOnly)
	seal := validSeal(ref, stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != task.VerdictCommitted || result.Refusal != "" || out.String() != "answer from Pi" {
		t.Fatalf("result=%+v output=%q", result, out.String())
	}
	if result.Session == nil || result.Session.Provider != Provider || result.Session.ConversationID != testUUID {
		t.Fatalf("session=%+v", result.Session)
	}
	if len(result.Usage) != 1 || result.Usage[0].Scope != task.UsageScopeUnknown || result.Usage[0].InputTokens == nil || *result.Usage[0].InputTokens != 3 {
		t.Fatalf("usage=%+v", result.Usage)
	}
}

func TestPiInterpreterAcceptsNativeSettledCompletion(t *testing.T) {
	// Pi JSON mode emits agent_settled immediately after agent_end for a
	// successful noninteractive invocation. It is a terminal lifecycle marker;
	// the validated answer still comes from turn_end and agent_end.
	stdout := append(piStream(testUUID, "settled answer", true), []byte("\n{\"type\":\"agent_settled\"}")...)
	seal := validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil || result.Verdict != task.VerdictCommitted || out.String() != "settled answer" {
		t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
	}
}

func TestPiInterpreterAcceptsMultipleToolTurns(t *testing.T) {
	stdout := piToolStream(testUUID, "final after tool")
	ref := ReferenceForMode(ModeReadOnly)
	seal := validSeal(ref, stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil || result.Verdict != task.VerdictCommitted || out.String() != "final after tool" {
		t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
	}
}

func TestPiInterpreterAcceptsNativeMCPEventAlongsideToolTurn(t *testing.T) {
	stdout := append([]byte(`{"type":"mcp_tool_call","server":"fixture","tool":"read"}`+"\n"), piToolStream(testUUID, "final after MCP")...)
	ref := ReferenceForMode(ModeReadOnly)
	seal := validSeal(ref, stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil || result.Verdict != task.VerdictCommitted || out.String() != "final after MCP" {
		t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
	}
}

func TestPiAcceptsEmptyTextInIntermediateToolResult(t *testing.T) {
	message := []byte(`{"role":"toolResult","content":[{"type":"text","text":""}],"toolCallId":"call-1","isError":false}`)
	parsed, err := decodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.role != "toolResult" || len(parsed.text) != 0 {
		t.Fatalf("parsed intermediate result=%+v", parsed)
	}
}

func TestPiInterpreterRejectsUnsuccessfulAssistantStopReasons(t *testing.T) {
	for _, reason := range []string{"length", "error", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			message := fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"partial"}],"stopReason":%q,"errorMessage":"provider stopped"}`, reason)
			lines := []string{
				sessionLine(testUUID),
				`{"type":"agent_start"}`,
				`{"type":"turn_start"}`,
				`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
				fmt.Sprintf(`{"type":"message_end","message":%s}`, message),
				fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, message),
				fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, message),
			}
			stdout := []byte(strings.Join(lines, "\n"))
			seal := validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)
			var out bytes.Buffer
			result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
			if err != nil || result.Verdict != task.VerdictRejected || out.Len() != 0 || result.Refusal != refusalProviderFailed {
				t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
			}
		})
	}
}

func TestPiInterpreterRejectsAssistantErrorPayloadWithSuccessfulStop(t *testing.T) {
	stdout := []byte(strings.ReplaceAll(string(piStream(testUUID, "claimed answer", true)),
		`"stopReason":"stop"`, `"stopReason":"stop","errorMessage":"provider reported an error"`))
	seal := validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalProviderFailed || out.Len() != 0 {
		t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
	}
}

func TestPiInterpreterRejectsToolUseAsFinalAssistantStopReason(t *testing.T) {
	toolMessage := `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{}}],"stopReason":"toolUse"}`
	lines := []string{
		sessionLine(testUUID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, toolMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, toolMessage),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, toolMessage),
	}
	stdout := []byte(strings.Join(lines, "\n"))
	seal := validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)
	var out bytes.Buffer
	result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &out)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed || out.Len() != 0 {
		t.Fatalf("result=%+v err=%v output=%q", result, err, out.String())
	}
}

func TestPiInterpreterRejectsAgentEndAfterNonterminalTurn(t *testing.T) {
	toolMessage := `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"read","arguments":{}}],"stopReason":"toolUse"}`
	finalMessage := `{"role":"assistant","content":[{"type":"text","text":"claimed final"}],"stopReason":"stop"}`
	stdout := []byte(strings.Join([]string{
		sessionLine(testUUID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, toolMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, toolMessage),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s,%s]}`, toolMessage, finalMessage),
	}, "\n"))
	result, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)}, evidence{stdout: stdout}, io.Discard,
	)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPiInterpreterRejectsInvalidTerminalAndIdentityEvidence(t *testing.T) {
	cases := []struct {
		name    string
		stdout  []byte
		expect  task.SessionExpectation
		refusal string
	}{
		{name: "missing terminal", stdout: piStream(testUUID, "answer", false), refusal: refusalIncomplete},
		{name: "wrong continuation", stdout: piStream(testUUID, "answer", true), expect: task.SessionExpectation{Required: true, ID: "123e4567-e89b-12d3-a456-426614174001"}, refusal: refusalIdentity},
		{name: "malformed", stdout: append(piStream(testUUID, "answer", true), '\n', '{'), refusal: refusalMalformed},
		{name: "empty answer", stdout: piStream(testUUID, "   ", true), refusal: refusalEmpty},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ref := ReferenceForMode(ModeReadOnly)
			seal := validSeal(ref, testCase.stdout, nil)
			result, err := NewInterpreter(ModeReadOnly).Evaluate(predicate.Input{Seal: seal, ExpectedSession: testCase.expect}, evidence{stdout: testCase.stdout}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if result.Verdict != task.VerdictRejected || result.Refusal != testCase.refusal {
				t.Fatalf("result=%+v want refusal=%q", result, testCase.refusal)
			}
		})
	}
}

func TestPiInterpreterRejectsConflictingFinalText(t *testing.T) {
	stdout := piStream(testUUID, "answer", true)
	stdout = append(stdout, []byte(fmt.Sprintf("\n{\"type\":\"telemetry\",\"message\":%q}", "ok"))...)
	ref := ReferenceForMode(ModeReadOnly)
	seal := validSeal(ref, stdout, nil)
	result, err := NewInterpreter().Evaluate(predicate.Input{Seal: seal}, evidence{stdout: stdout}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != task.VerdictCommitted {
		t.Fatalf("benign trailing telemetry result=%+v", result)
	}
}

func TestPiInterpreterRejectsConflictingEmptyCompletedMessage(t *testing.T) {
	emptyMessage := `{"role":"assistant","content":[],"stopReason":"stop"}`
	nonemptyMessage := `{"role":"assistant","content":[{"type":"text","text":"late text"}],"stopReason":"stop"}`
	stdout := []byte(strings.Join([]string{
		sessionLine(testUUID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, emptyMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, nonemptyMessage),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, nonemptyMessage),
	}, "\n"))
	result, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)}, evidence{stdout: stdout}, io.Discard,
	)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPiInterpreterRejectsAgentEndTextWhenTurnTextIsEmpty(t *testing.T) {
	emptyMessage := `{"role":"assistant","content":[],"stopReason":"stop"}`
	nonemptyMessage := `{"role":"assistant","content":[{"type":"text","text":"late text"}],"stopReason":"stop"}`
	stdout := []byte(strings.Join([]string{
		sessionLine(testUUID),
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		fmt.Sprintf(`{"type":"message_end","message":%s}`, emptyMessage),
		fmt.Sprintf(`{"type":"turn_end","message":%s,"toolResults":[]}`, emptyMessage),
		fmt.Sprintf(`{"type":"agent_end","messages":[%s]}`, nonemptyMessage),
	}, "\n"))
	result, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)}, evidence{stdout: stdout}, io.Discard,
	)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPiInterpreterRejectsUnknownLifecycleAfterAgentEnd(t *testing.T) {
	stdout := append(piStream(testUUID, "answer", true), []byte("\n{\"type\":\"agent_interrupted\"}")...)
	result, err := NewInterpreter(ModeReadOnly).Evaluate(
		predicate.Input{Seal: validSeal(ReferenceForMode(ModeReadOnly), stdout, nil)}, evidence{stdout: stdout}, io.Discard,
	)
	if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPiInterpreterBoundsUsageRows(t *testing.T) {
	state := eventState{}
	usage := &usageTotals{present: true}
	for range maxUsageRows {
		appendUsage(&state, usage)
	}
	if state.semanticErr != nil || len(state.usage) != maxUsageRows {
		t.Fatalf("before bound state=%+v", state)
	}
	appendUsage(&state, usage)
	if state.semanticErr == nil || len(state.usage) != maxUsageRows {
		t.Fatalf("after bound state=%+v", state)
	}
}
