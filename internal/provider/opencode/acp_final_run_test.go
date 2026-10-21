package opencode

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	acpImageContent = `{"type":"image","data":"AAAA","mimeType":"image/png"}`
	// acpCase13Preamble and acpCase13Marker are sanitized stand-ins with the
	// sizes of the v0.1.33 native case 13 evidence: a 37-byte preamble
	// message and a distinct 84-byte answer message.
	acpCase13Preamble = "I'll read the acceptance marker file."
	acpCase13Marker   = "OPENCODE-ACCEPTANCE-MARKER-5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e\n"
)

// acpMessageChunk renders a root agent_message_chunk whose messageId member
// is the raw JSON token id, or has no messageId when id is empty.
func acpMessageChunk(id, content string) string {
	member := ""
	if id != "" {
		member = `"messageId":` + id + `,`
	}
	return acpUpdate(acpRootSession, `{"sessionUpdate":"agent_message_chunk",`+member+`"content":`+content+`}`)
}

func acpText(id, text string) string {
	return acpMessageChunk(id, `{"type":"text","text":`+acpQuote(text)+`}`)
}

func acpTool(update, status string) string {
	return acpUpdate(acpRootSession, `{"sessionUpdate":`+acpQuote(update)+`,"toolCallId":"call_read","title":"read","kind":"read","status":`+acpQuote(status)+`}`)
}

// acpCase13Body is the sanitized shape of the sealed case 13 prompt: a root
// preamble message, the read tool lifecycle, then a distinct root message
// carrying the exact answer split into two chunks.
func acpCase13Body() []string {
	return []string{
		acpText(`"msg_case13_preamble"`, acpCase13Preamble),
		acpTool("tool_call", "pending"), acpTool("tool_call_update", "in_progress"), acpTool("tool_call_update", "completed"),
		acpText(`"msg_case13_answer"`, acpCase13Marker[:40]),
		acpText(`"msg_case13_answer"`, acpCase13Marker[40:]),
	}
}

// evaluateSelection interprets stdout under the ACP contract revision
// selection, sealed with that revision's reference.
func evaluateSelection(t *testing.T, selection acpSelection, exchange acpExchange, stdout []byte, out io.Writer) task.Interpretation {
	t.Helper()
	interpreter := acpInterpreter{mode: exchange.mode, selection: selection}
	input := acpInput(exchange, stdout)
	input.Seal.Predicate = interpreter.Reference()
	result, err := interpreter.Evaluate(input, &testEvidence{stdout: stdout}, out)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.ValidateOutputDiagnostics(result.Diagnostics); err != nil {
		t.Fatalf("invalid diagnostics %+v: %v", result.Diagnostics, err)
	}
	return result
}

func TestACPV2CommitsTheFinalMessageOfNativeCase13(t *testing.T) {
	if len(acpCase13Preamble) != 37 || len(acpCase13Marker) != 84 {
		t.Fatalf("fixture sizes %d/%d do not match the sealed evidence", len(acpCase13Preamble), len(acpCase13Marker))
	}
	exchange := acpExchange{mode: ModeReadOnly, model: "opencode/big-pickle"}
	lines := exchange.stream("end_turn", acpCase13Body()...)
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	driver.mustFeed(lines...)
	if err := driver.protocol.Complete(); err != nil {
		t.Fatalf("live protocol did not complete: %v", err)
	}
	var answer bytes.Buffer
	result := evaluateSelection(t, acpSelectFinalRun, exchange, acpStdout(lines), &answer)
	if result.Verdict != task.VerdictCommitted || answer.String() != acpCase13Marker || len(result.Diagnostics) != 0 {
		t.Fatalf("acp-v2 verdict=%q answer=%d bytes %q diagnostics=%+v", result.Verdict, answer.Len(), answer.String(), result.Diagnostics)
	}
	answer.Reset()
	result = evaluateSelection(t, acpSelectWholeTurn, exchange, acpStdout(lines), &answer)
	if result.Verdict != task.VerdictCommitted || answer.String() != acpCase13Preamble+acpCase13Marker || answer.Len() != 121 {
		t.Fatalf("acp-v1 must keep its whole-turn answer: verdict=%q answer=%d bytes", result.Verdict, answer.Len())
	}
}

type acpSelectionCase struct {
	body       []string
	answer     string
	refusal    string
	diagnostic string
}

func acpSelectionCases() map[string]acpSelectionCase {
	large := strings.Repeat("i", 3*1024*1024)
	return map[string]acpSelectionCase{
		"one message":                  {body: []string{acpText(`"A"`, "x"), acpText(`"A"`, "y")}, answer: "xy"},
		"tool then a new message":      {body: []string{acpText(`"A"`, "pre"), acpTool("tool_call", "completed"), acpText(`"B"`, "fi"), acpText(`"B"`, "nal")}, answer: "final"},
		"tool inside one message":      {body: []string{acpText(`"A"`, "a"), acpTool("tool_call", "completed"), acpText(`"A"`, "b")}, answer: "ab"},
		"advisory updates do not end":  {body: []string{acpText(`"A"`, "a"), acpThought(acpRootSession), acpChildChunk(acpRootSession, "CHILD"), acpUpdate(acpRootSession, `{"sessionUpdate":"plan","entries":[]}`), acpText(`"A"`, "b")}, answer: "ab"},
		"final id reappears":           {body: []string{acpText(`"A"`, "1"), acpText(`"B"`, "2"), acpText(`"A"`, "3")}, answer: "123", diagnostic: "answer_message_boundary_ambiguous"},
		"only the final run":           {body: []string{acpText(`"A"`, "1"), acpText(`"B"`, "2"), acpText(`"A"`, "3"), acpText(`"C"`, "4"), acpText(`"C"`, "5")}, answer: "45"},
		"missing id":                   {body: []string{acpText(`"A"`, "1"), acpText("", "2"), acpText(`"B"`, "3")}, answer: "123", diagnostic: "answer_message_boundary_unavailable"},
		"null id":                      {body: []string{acpText(`"A"`, "1"), acpText("null", "2")}, answer: "12", diagnostic: "answer_message_boundary_unavailable"},
		"empty id is one message":      {body: []string{acpText(`""`, "a"), acpText(`""`, "b")}, answer: "ab"},
		"empty id is an id":            {body: []string{acpText(`""`, "pre"), acpText(`"A"`, "fin")}, answer: "fin"},
		"equivalent escapes":           {body: []string{acpText(`"msg_A"`, "a"), acpText(`"msg_\u0041"`, "b"), acpText(`"\u006dsg_A"`, "c")}, answer: "abc"},
		"same length different ids":    {body: []string{acpText(`"msg_AAAA"`, "pre"), acpText(`"msg_AAAB"`, "fin")}, answer: "fin"},
		"large ids":                    {body: []string{acpText(acpQuote(large+"1"), "pre"), acpText(acpQuote(large+"2"), "fi"), acpText(acpQuote(large+"2"), "n")}, answer: "fin"},
		"nontext chunk joins its run":  {body: []string{acpText(`"A"`, "pre"), acpMessageChunk(`"B"`, acpImageContent), acpText(`"B"`, "fin")}, answer: "fin"},
		"child and thought ids ignore": {body: []string{acpText(`"A"`, "a"), acpChildChunk(acpRootSession, "x"), acpText(`"A"`, "b")}, answer: "ab"},
		"blank final run":              {body: []string{acpText(`"A"`, "preamble"), acpText(`"B"`, " \n\t")}, refusal: refusalEmpty},
		"nontext final run":            {body: []string{acpText(`"A"`, "preamble"), acpMessageChunk(`"B"`, acpImageContent)}, refusal: refusalEmpty},
		"no root message":              {refusal: refusalEmpty},
		"child only":                   {body: []string{acpChildChunk(acpRootSession, "child answer")}, refusal: refusalEmpty},
		"blank whole-turn fallback":    {body: []string{acpText("", " ")}, refusal: refusalEmpty, diagnostic: "answer_message_boundary_unavailable"},
		"numeric id":                   {body: []string{acpText(`"A"`, "pre"), acpText(`7`, "fin")}, refusal: refusalMalformed},
		"object id":                    {body: []string{acpText(`{"id":"A"}`, "fin")}, refusal: refusalMalformed},
		"boolean id beside missing":    {body: []string{acpText("", "a"), acpText(`true`, "b")}, refusal: refusalMalformed},
		"duplicated id":                {body: []string{acpText(`"A","messageId":"B"`, "fin")}, refusal: refusalMalformed},
	}
}

func TestACPV2SelectsTheFinalRootMessageRun(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite}
	for name, testCase := range acpSelectionCases() {
		t.Run(name, func(t *testing.T) {
			var answer bytes.Buffer
			result := evaluateSelection(t, acpSelectFinalRun, exchange, acpStdout(exchange.stream("end_turn", testCase.body...)), &answer)
			if testCase.refusal != "" {
				if result.Verdict != task.VerdictRejected || result.Refusal != testCase.refusal || answer.Len() != 0 {
					t.Fatalf("verdict=%q refusal=%q answer=%q, want rejected %q", result.Verdict, result.Refusal, answer.String(), testCase.refusal)
				}
			} else if result.Verdict != task.VerdictCommitted || answer.String() != testCase.answer {
				t.Fatalf("verdict=%q refusal=%q answer=%d bytes, want %q", result.Verdict, result.Refusal, answer.Len(), testCase.answer)
			}
			if got := acpSelectionDiagnostic(result); got != testCase.diagnostic {
				t.Fatalf("selection diagnostic=%q want %q (all %+v)", got, testCase.diagnostic, result.Diagnostics)
			}
		})
	}
}

func acpSelectionDiagnostic(result task.Interpretation) string {
	for _, diagnostic := range result.Diagnostics {
		if strings.HasPrefix(diagnostic.Code, "answer_message_boundary_") {
			return diagnostic.Code
		}
	}
	return ""
}

// TestACPMalformedMessageIDRefusesOnlyTheSealedV2Selection proves that a
// selector-specific fault never reaches the live reducer: the live protocol
// and acp-v1 complete, and only the sealed acp-v2 selection refuses.
func TestACPMalformedMessageIDRefusesOnlyTheSealedV2Selection(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	for name, id := range map[string]string{"number": `7`, "object": `{}`, "array": `["A"]`, "boolean": `false`, "duplicate": `"A","messageId":"A"`} {
		t.Run(name, func(t *testing.T) {
			lines := exchange.stream("end_turn", acpText(`"A"`, "pre"), acpText(id, "DONE"))
			driver := newACPDriver(t, exchange, nil)
			driver.start(acpTestBrief)
			driver.mustFeed(lines...)
			if err := driver.protocol.Complete(); err != nil {
				t.Fatalf("live protocol was poisoned by selector metadata: %v", err)
			}
			var answer bytes.Buffer
			if result := evaluateSelection(t, acpSelectWholeTurn, exchange, acpStdout(lines), &answer); result.Verdict != task.VerdictCommitted || answer.String() != "preDONE" {
				t.Fatalf("acp-v1 verdict=%q answer=%q", result.Verdict, answer.String())
			}
			answer.Reset()
			result := evaluateSelection(t, acpSelectFinalRun, exchange, acpStdout(lines), &answer)
			if result.Verdict != task.VerdictRejected || result.Refusal != refusalMalformed || answer.Len() != 0 || result.Diagnostics[0].Code != "invalid_output" {
				t.Fatalf("acp-v2 verdict=%q refusal=%q diagnostics=%+v", result.Verdict, result.Refusal, result.Diagnostics)
			}
		})
	}
}

// TestACPV2TurnRefusalsPrecedeAnswerSelection keeps every completion,
// session, seal and late-record fault ahead of message selection.
func TestACPV2TurnRefusalsPrecedeAnswerSelection(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	setup, _ := exchange.setup()
	runs := []string{acpText(`"A"`, "pre"), acpText(`7`, "malformed"), acpText(`"B"`, "fin")}
	cases := map[string]struct {
		lines   []string
		seal    func(*task.ProviderExitRecord)
		refusal string
	}{
		"stop reason":      {lines: exchange.stream("max_tokens", runs...), refusal: refusalProviderFailed + ": stop_reason=max_tokens"},
		"no terminal":      {lines: extendLines(setup, runs...), refusal: refusalIncomplete},
		"session mismatch": {lines: exchange.stream("end_turn", append(extendLines(runs), acpRootChunk("ses_other000000000000000000000", "x"))...), refusal: refusalIdentity},
		"late root text":   {lines: append(exchange.stream("end_turn", runs...), acpText(`"B"`, "late")), refusal: refusalMalformed},
		"seal error":       {lines: exchange.stream("end_turn", runs...), seal: func(seal *task.ProviderExitRecord) { seal.Error = "native protocol did not complete" }},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			stdout := acpStdout(testCase.lines)
			input := acpInput(exchange, stdout)
			if testCase.seal != nil {
				testCase.seal(&input.Seal)
				testCase.refusal = sealRefusal(input.Seal)
			}
			var answer bytes.Buffer
			result, err := NewInterpreter(exchange.mode).Evaluate(input, &testEvidence{stdout: stdout}, &answer)
			if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != testCase.refusal || answer.Len() != 0 || acpSelectionDiagnostic(result) != "" {
				t.Fatalf("err=%v verdict=%q refusal=%q want %q answer=%d diagnostics=%+v", err, result.Verdict, result.Refusal, testCase.refusal, answer.Len(), result.Diagnostics)
			}
		})
	}
}

// replayEvidence serves stdout per replay pass, counted by stdout reads: a
// pass beyond the list repeats the last stdout, and a pass in fail reads
// half of its stdout and then fails.
type replayEvidence struct {
	stdout [][]byte
	fail   map[int]error
	reads  int
}

func (e *replayEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if stream != predicate.Stdout {
		return consume(bytes.NewReader(nil))
	}
	pass := e.reads
	e.reads++
	data := e.stdout[min(pass, len(e.stdout)-1)]
	if err := e.fail[pass]; err != nil {
		return consume(&errorReader{data: data[:len(data)/2], err: err})
	}
	return consume(bytes.NewReader(data))
}

func TestACPV2UsesThreeReplaysAndKeepsOperationalFaultsSeparate(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	stdout := acpStdout(exchange.stream("end_turn", acpCase13Body()...))
	evidence := &replayEvidence{stdout: [][]byte{stdout}}
	var answer bytes.Buffer
	result, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), evidence, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != acpCase13Marker || evidence.reads != 3 {
		t.Fatalf("err=%v verdict=%q answer=%q stdout replays=%d, want 3", err, result.Verdict, answer.String(), evidence.reads)
	}
	readErr := errors.New("evidence read failed")
	for _, pass := range []int{0, 1, 2} {
		evidence := &replayEvidence{stdout: [][]byte{stdout}, fail: map[int]error{pass: readErr}}
		if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), evidence, io.Discard); !errors.Is(err, readErr) || errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("read failure in replay %d error=%v", pass+1, err)
		}
	}
	writeErr := errors.New("payload writer failed")
	if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), &testEvidence{stdout: stdout}, &failingWriter{err: writeErr}); !errors.Is(err, writeErr) {
		t.Fatalf("answer writer failure error=%v", err)
	}
}

// offsetFaultReader fails a positioned read at exactly one offset. Only the
// selector starts a read at a messageId value, so the fault reaches it alone.
type offsetFaultReader struct {
	*bytes.Reader
	at  int64
	err error
}

func (r *offsetFaultReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset == r.at {
		return 0, r.err
	}
	return r.Reader.ReadAt(p, offset)
}

func TestACPV2MessageIDReadFailureIsOperational(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	stdout := acpStdout(exchange.stream("end_turn", acpText(`"msg_read_fault"`, "DONE")))
	at := bytes.Index(stdout, []byte(`"messageId":"msg_read_fault"`)) + len(`"messageId":`)
	readErr := errors.New("messageId read failed")
	evidence := &testEvidence{stdoutReader: &offsetFaultReader{Reader: bytes.NewReader(stdout), at: int64(at), err: readErr}}
	var answer bytes.Buffer
	if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), evidence, &answer); !errors.Is(err, readErr) || answer.Len() != 0 {
		t.Fatalf("messageId read failure error=%v answer=%d bytes", err, answer.Len())
	}
}

func TestACPV2ReplaysThatDisagreeAreEvidenceFaults(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	stream := func(body ...string) []byte { return acpStdout(exchange.stream("end_turn", body...)) }
	first := stream(acpText(`"A"`, "1"), acpText(`"B"`, "2"), acpText(`"C"`, "3"))
	cases := map[string][][]byte{
		"second replay final run differs":   {first, stream(acpText(`"A"`, "1"), acpText(`"D"`, "2"))},
		"second replay blankness differs":   {first, stream(acpText(`"A"`, "1"), acpText(`"B"`, "2"), acpText(`"C"`, " "))},
		"third replay repetition differs":   {first, first, stream(acpText(`"C"`, "1"), acpText(`"B"`, "2"), acpText(`"C"`, "3"))},
		"third replay boundaries disappear": {first, first, stream(acpText(`"A"`, "1"), acpText(`"B"`, "2"), acpText("", "3"))},
	}
	for name, passes := range cases {
		t.Run(name, func(t *testing.T) {
			evidence := &replayEvidence{stdout: passes}
			if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, first), evidence, io.Discard); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("disagreeing replays error=%v", err)
			}
		})
	}
}

func TestACPV2StreamsTheFinalRunInBoundedChunks(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	text := strings.Repeat("a", 2*1024*1024)
	stdout := acpStdout(exchange.stream("end_turn", acpText(`"A"`, text), acpText(`"B"`, text), acpText(`"B"`, text)))
	sink := &opencodeCountingWriter{}
	result := evaluateSelection(t, acpSelectFinalRun, exchange, stdout, sink)
	if result.Verdict != task.VerdictCommitted || sink.count != 2*len(text) || sink.max > 64*1024 {
		t.Fatalf("verdict=%q streamed=%d (largest write %d) want %d in bounded chunks", result.Verdict, sink.count, sink.max, 2*len(text))
	}
	// Run facts are fixed-size values: no ID bytes, answer text, or
	// per-message collection is retained, whatever the stream length.
	for _, value := range []any{acpRunFacts{}, acpRunTarget{}} {
		requireFixedSize(t, reflect.TypeOf(value))
	}
}

func requireFixedSize(t *testing.T, kind reflect.Type) {
	t.Helper()
	if kind.Kind() == reflect.Struct {
		for index := range kind.NumField() {
			requireFixedSize(t, kind.Field(index).Type)
		}
		return
	}
	if kind.Kind() == reflect.Array {
		requireFixedSize(t, kind.Elem())
		return
	}
	if !slices.Contains([]reflect.Kind{reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint8}, kind.Kind()) {
		t.Fatalf("%s retains variable-size state", kind)
	}
}

// TestACPV1AndV2EvidenceKeepTheirRecordedInterpreter refuses evidence sealed
// under one ACP revision when it is presented to the other.
func TestACPV1AndV2EvidenceKeepTheirRecordedInterpreter(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite}
	stdout := acpStdout(exchange.stream("end_turn", acpRootChunk(acpRootSession, "DONE")))
	for _, pair := range [][2]acpSelection{{acpSelectWholeTurn, acpSelectFinalRun}, {acpSelectFinalRun, acpSelectWholeTurn}} {
		input := acpInput(exchange, stdout)
		input.Seal.Predicate = acpReference(pair[0], exchange.mode)
		interpreter := acpInterpreter{mode: exchange.mode, selection: pair[1]}
		if _, err := interpreter.Evaluate(input, &testEvidence{stdout: stdout}, io.Discard); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("evidence sealed under %s accepted by %s: %v", input.Seal.Predicate.Version, interpreter.Reference().Version, err)
		}
	}
	if acpV1ReferenceForMode(exchange.mode).Version != acpV1PredicateVersion || ReferenceForMode(exchange.mode).Version != acpPredicateVersion {
		t.Fatal("ACP revisions are not acp-v1 and current acp-v2")
	}
}
