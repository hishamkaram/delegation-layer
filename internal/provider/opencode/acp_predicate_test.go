package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TestPredicateContractDigestsArePinned binds every registered OpenCode
// contract to its exact bytes. Historical run revisions never change; the ACP
// revision is new.
func TestPredicateContractDigestsArePinned(t *testing.T) {
	want := map[string]map[string]string{
		ModeReadOnly: {
			"acp-v1": "84091a88654cbbef98c32c623559b98a510f5edc7e7efc7df47373a47a8115ca",
			"5":      "a5908a2a500a4fc4203b6b16d403e3b2f350380645ee38b4fcdd445ea03b1a3f",
			"4":      "b375397104da62801651c8ad3c2c9f20d6144436264b276b47ae94e2287d7c20",
			"3":      "4fce81c253212816c6a775938cd2194ad8cac29bfe69ae77a78ae025b170c518",
			"2":      "b097c597256043d5776a6fdd7ff526b76cfa19ec058b15ac5160aa54fb1d7352",
			"1":      "b81dbba63db7fe7b902a2cbefaeb277cae79378b3823cd68bfd44989188fa7ec",
		},
		ModeWorkspaceWrite: {
			"acp-v1": "10d0b4534c133b94adebcb4c4403bd25ae5af9da50732cd28d2f30712d1c1059",
			"5":      "da63dc56abdbe64127c204b3e3afddb3f41f7b9ce12d2d6b2a7c397cee41c8be",
			"4":      "1bdc5fea3de999a79475b080da565e5c74807f1e7d1b44e944002036772e58c9",
			"3":      "7b39a6f70ed5dd6d4a2605ebf5ebe7dc81f4cf7a10e7a95cc5d807baa7666ca6",
			"2":      "08226cfd57f8c882b5866813c849fb06bd802c82d94c992b8e540407e606b147",
			"1":      "31d171b320f766d90e14e23cc6eccf6ce63c5135b70c010f75ec389ffb4d986a",
		},
	}
	for mode, digests := range want {
		for _, reference := range []task.PredicateRef{
			ReferenceForMode(mode), runReferenceForMode(mode), historicalCurrentV4ReferenceForMode(mode),
			historicalCurrentReferenceForMode(mode), historicalReferenceForMode(mode), LegacyReferenceForMode(mode),
		} {
			if reference.Adapter != Provider || reference.Mode != mode || reference.SHA256 != digests[reference.Version] {
				t.Errorf("%s v%s digest=%s want %s", mode, reference.Version, reference.SHA256, digests[reference.Version])
			}
		}
		if contract := ContractForMode(mode); task.ComputeSHA256([]byte(contract)) != digests["acp-v1"] || !strings.HasSuffix(contract, "\n") {
			t.Errorf("%s current contract is not the pinned ACP contract", mode)
		}
	}
	if ReferenceForMode("unknown") != (task.PredicateRef{}) {
		t.Error("unknown mode produced a reference")
	}
}

// extendLines returns a new slice; shared fixture lines are never mutated.
func extendLines(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

func acpSeal(stdout []byte, mode string) task.ProviderExitRecord {
	seal := validSeal(stdout, mode)
	seal.Predicate = acpReferenceForMode(mode)
	return seal
}

// evaluateACP interprets stdout as sealed evidence of exchange.
func evaluateACP(t *testing.T, exchange acpExchange, stdout []byte, out io.Writer) task.Interpretation {
	t.Helper()
	result, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), &testEvidence{stdout: stdout}, out)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func acpInput(exchange acpExchange, stdout []byte) predicate.Input {
	input := predicate.Input{Seal: acpSeal(stdout, exchange.mode), RequestedConfig: exchange.config()}
	if exchange.expected != "" {
		input.ExpectedSession = task.SessionExpectation{Required: true, ID: exchange.expected}
	}
	return input
}

func TestACPInterpreterCommitsOrderedRootAnswerOnly(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly, model: "opencode/big-pickle", effort: "high"}
	stdout := acpStdout(exchange.stream("end_turn",
		acpThought(acpRootSession), acpRootChunk(acpRootSession, "ROOT-"), acpChildChunk(acpRootSession, "CHILD"),
		acpRootChunk(acpRootSession, "SUM\n"), acpUpdate(acpRootSession, `{"sessionUpdate":"usage_update","used":1,"size":2}`),
	))
	var answer bytes.Buffer
	result := evaluateACP(t, exchange, stdout, &answer)
	if result.Verdict != task.VerdictCommitted || answer.String() != "ROOT-SUM\n" {
		t.Fatalf("verdict=%q refusal=%q answer=%q", result.Verdict, result.Refusal, answer.String())
	}
	if result.Session == nil || *result.Session != (task.SessionIdentity{Provider: Provider, ConversationID: acpRootSession}) {
		t.Fatalf("session=%+v", result.Session)
	}
	if len(result.Usage) != 1 || result.Usage[0].Scope != task.UsageScopeUnknown || result.Usage[0].Reliability != task.UsageReliabilityReported {
		t.Fatalf("usage=%+v", result.Usage)
	}
	usage := result.Usage[0]
	assertInt(t, "input", usage.InputTokens, 13083)
	assertInt(t, "thinking", usage.ThinkingTokens, 35)
	assertInt(t, "cache read", usage.CacheReadTokens, 15616)
	if usage.CacheCreationTokens != nil || task.ValidateUsageMetadata(&usage) != nil {
		t.Fatalf("absent counter was synthesized or usage is invalid: %+v", usage)
	}
}

// TestACPInterpreterReplaysTheLiveVerdict feeds the same stream to the live
// protocol and to the sealed-evidence interpreter: both reach one verdict.
func TestACPInterpreterReplaysTheLiveVerdict(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite, expected: testSession}
	streams := map[string][]string{
		"success":      exchange.stream("end_turn", acpPermissionRequest("0", testSession, "allow_once", "reject_once"), acpRootChunk(testSession, "DONE")),
		"refusal":      exchange.stream("refusal", acpRootChunk(testSession, "no")),
		"trailing":     append(exchange.stream("end_turn", acpRootChunk(testSession, "DONE")), acpRootChunk(testSession, "late")),
		"partial EOF":  exchange.stream("end_turn")[:4],
		"unknown kind": exchange.stream("end_turn", acpUpdate(testSession, `{"sessionUpdate":"novel"}`), acpRootChunk(testSession, "ok")),
	}
	for name, lines := range streams {
		t.Run(name, func(t *testing.T) {
			driver := newACPDriver(t, exchange, nil)
			driver.start(acpTestBrief)
			feedErr := driver.feed(lines...)
			result := evaluateACP(t, exchange, acpStdout(lines), io.Discard)
			live := feedErr == nil && driver.protocol.Complete() == nil && driver.protocol.session.answer.nonblank
			if live != (result.Verdict == task.VerdictCommitted) || (feedErr != nil) != (result.Refusal == refusalMalformed) {
				t.Fatalf("live feed=%v complete=%v but replay verdict=%q refusal=%q", feedErr, driver.protocol.Complete(), result.Verdict, result.Refusal)
			}
		})
	}
}

func TestACPInterpreterRejectsEveryIncompleteOrFailedTurn(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	lines, promptID := exchange.setup()
	cases := map[string]struct {
		lines   []string
		refusal string
	}{
		"partial EOF with text":   {extendLines(lines, acpRootChunk(acpRootSession, "looks done")), refusalIncomplete},
		"no output":               {nil, refusalIncomplete},
		"max tokens":              {exchange.stream("max_tokens", acpRootChunk(acpRootSession, "cut")), refusalProviderFailed + ": stop_reason=max_tokens"},
		"cancelled":               {exchange.stream("cancelled", acpRootChunk(acpRootSession, "cut")), refusalProviderFailed + ": stop_reason=cancelled"},
		"native error":            {extendLines(lines, acpErrorResult(promptID, -32603, "secret upstream detail")), refusalProviderFailed + ": native_error"},
		"unacknowledged":          {extendLines(lines[:3], acpConfigResult(3, "mode=build")), refusalProviderFailed + ": configuration_unacknowledged"},
		"child-only answer":       {exchange.stream("end_turn", acpChildChunk(acpRootSession, "child answer")), refusalEmpty},
		"blank answer":            {exchange.stream("end_turn", acpRootChunk(acpRootSession, " \n\t")), refusalEmpty},
		"no answer":               {exchange.stream("end_turn"), refusalEmpty},
		"malformed record":        {extendLines(lines, `{"jsonrpc":"2.0"`), refusalMalformed},
		"trailing request":        {extendLines(exchange.stream("end_turn", acpRootChunk(acpRootSession, "ok")), acpPermissionRequest("1", acpRootSession, "reject_once")), refusalMalformed},
		"session mismatch":        {exchange.stream("end_turn", acpRootChunk("ses_other000000000000000000000", "x")), refusalIdentity},
		"wrong response ordering": {extendLines(lines[:1], acpConfigResult(2, "mode=plan")), refusalMalformed},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var answer bytes.Buffer
			result := evaluateACP(t, exchange, acpStdout(testCase.lines), &answer)
			if result.Verdict != task.VerdictRejected || result.Refusal != testCase.refusal || answer.Len() != 0 {
				t.Fatalf("verdict=%q refusal=%q answer=%d bytes, want rejected %q", result.Verdict, result.Refusal, answer.Len(), testCase.refusal)
			}
			if len(result.Diagnostics) == 0 || strings.Contains(result.Diagnostics[0].Message, "secret") || strings.Contains(result.Refusal, "secret") {
				t.Fatalf("diagnostics=%+v", result.Diagnostics)
			}
		})
	}
}

func TestACPInterpreterAppliesSealAndIdentityChecks(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite, expected: testSession}
	stdout := acpStdout(exchange.stream("end_turn", acpRootChunk(testSession, "DONE")))
	mutate := map[string]func(*predicate.Input){
		"nonzero exit": func(input *predicate.Input) { input.Seal.ExitCode = 1 },
		"seal error":   func(input *predicate.Input) { input.Seal.Error = "native protocol did not complete" },
		"recorded session": func(input *predicate.Input) {
			input.RecordedSession = &task.SessionIdentity{Provider: Provider, ConversationID: acpRootSession}
		},
		"fresh expected": func(input *predicate.Input) { input.ExpectedSession = task.SessionExpectation{} },
	}
	for name, change := range mutate {
		t.Run(name, func(t *testing.T) {
			input := acpInput(exchange, stdout)
			change(&input)
			var answer bytes.Buffer
			result, err := NewInterpreter(exchange.mode).Evaluate(input, &testEvidence{stdout: stdout}, &answer)
			if err != nil {
				t.Fatal(err)
			}
			if result.Verdict != task.VerdictRejected || answer.Len() != 0 {
				t.Fatalf("verdict=%q refusal=%q answer=%d", result.Verdict, result.Refusal, answer.Len())
			}
		})
	}
	input := acpInput(exchange, stdout)
	input.Seal.Predicate = runReferenceForMode(exchange.mode)
	if _, err := NewInterpreter(exchange.mode).Evaluate(input, &testEvidence{stdout: stdout}, io.Discard); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("run-sealed evidence under the ACP interpreter error=%v", err)
	}
	input = acpInput(exchange, stdout)
	input.RequestedConfig.Permission = ModeReadOnly
	if _, err := NewInterpreter(exchange.mode).Evaluate(input, &testEvidence{stdout: stdout}, io.Discard); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("request context for another mode error=%v", err)
	}
}

func TestACPInterpreterKeepsOperationalFaultsSeparate(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	stdout := acpStdout(exchange.stream("end_turn", acpRootChunk(acpRootSession, "DONE")))
	writeErr := errors.New("payload writer failed")
	if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), &testEvidence{stdout: stdout}, &failingWriter{err: writeErr}); !errors.Is(err, writeErr) {
		t.Fatalf("answer writer failure error=%v", err)
	}
	readErr := errors.New("evidence read failed")
	if _, err := NewInterpreter(exchange.mode).Evaluate(acpInput(exchange, stdout), &testEvidence{stdoutReader: &errorReader{data: stdout[:40], err: readErr}}, io.Discard); !errors.Is(err, readErr) {
		t.Fatalf("evidence read failure error=%v", err)
	}
}

func TestACPInterpreterIgnoresMalformedOptionalUsage(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	lines, promptID := exchange.setup()
	terminal := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn","usage":null}}`, promptID)
	result := evaluateACP(t, exchange, acpStdout(extendLines(lines, acpRootChunk(acpRootSession, "DONE"), terminal)), io.Discard)
	if result.Verdict != task.VerdictCommitted || len(result.Usage) != 0 || len(result.Diagnostics) != 0 {
		t.Fatalf("null usage is absent accounting: verdict=%q usage=%+v diagnostics=%+v", result.Verdict, result.Usage, result.Diagnostics)
	}
	for name, usage := range map[string]string{"negative": `{"inputTokens":-1}`, "string": `{"outputTokens":"many"}`, "array": `[]`} {
		t.Run(name, func(t *testing.T) {
			terminal := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn","usage":%s}}`, promptID, usage)
			stdout := acpStdout(append(append([]string(nil), lines...), acpRootChunk(acpRootSession, "DONE"), terminal))
			var answer bytes.Buffer
			result := evaluateACP(t, exchange, stdout, &answer)
			if result.Verdict != task.VerdictCommitted || answer.String() != "DONE" || len(result.Usage) != 0 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "optional_usage_unavailable" {
				t.Fatalf("verdict=%q usage=%+v diagnostics=%+v", result.Verdict, result.Usage, result.Diagnostics)
			}
		})
	}
}

func TestACPInterpreterStreamsLargeAnswerWithoutLineLimit(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	text := strings.Repeat("a", 2*1024*1024)
	stdout := acpStdout(exchange.stream("end_turn", acpRootChunk(acpRootSession, text), acpRootChunk(acpRootSession, text)))
	sink := &opencodeCountingWriter{}
	result := evaluateACP(t, exchange, stdout, sink)
	if result.Verdict != task.VerdictCommitted || sink.count != 2*len(text) || sink.max > 64*1024 {
		t.Fatalf("verdict=%q streamed=%d (largest write %d) want %d in bounded chunks", result.Verdict, sink.count, sink.max, 2*len(text))
	}
}
