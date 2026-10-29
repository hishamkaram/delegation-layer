package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// quotaProviderCanary stands for provider-owned quota values that must never
// reach a public diagnostic.
const quotaProviderCanary = "sk-ant-quota-canary"

// rateLimitLine is the sealed rate_limit_event wire shape: a structured
// rate_limit_info object with provider-owned reset, window, and overage values.
func rateLimitLine(status string) string {
	return rateLimitInfoLine(fmt.Sprintf(`{"status":%q,"rateLimitType":"five_hour","resetsAt":1793171400,"overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false,"unifiedWindows":[%q]}`, status, quotaProviderCanary))
}

func rateLimitInfoLine(info string) string {
	return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":%s,"uuid":"u-1","session_id":%q}`, info, claudeTestUUID)
}

// apiErrorResult is the sealed failed root result: subtype success with
// is_error true and terminal_reason api_error, carrying provider error text.
func apiErrorResult(attribution string) string {
	line := attributedResultLine("API Error: 429 "+quotaProviderCanary, attribution, 1, "0.1")
	line = mutateLine(line, `"is_error":false`, `"is_error":true`)
	line = mutateLine(line, `"stop_reason":"end_turn"`, `"stop_reason":"stop_sequence"`)
	return mutateLine(line, `"terminal_reason":"completed"`, `"terminal_reason":"api_error"`)
}

func evaluateQuotaTrace(t *testing.T, v predicate.Interpreter, lines []string, exitCode int) (task.Interpretation, string) {
	t.Helper()
	stdout := jsonl(lines...)
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	input.Seal.ExitCode = exitCode
	var answer bytes.Buffer
	interpretation, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}

// requireQuotaRejection requires a rejected provider failure whose first
// diagnostic is exactly want and whose public interpretation leaks no
// provider-owned quota value.
func requireQuotaRejection(t *testing.T, got task.Interpretation, answer string, want task.OutputDiagnostic) {
	t.Helper()
	if got.Verdict != task.VerdictRejected || !strings.HasPrefix(got.Refusal, refusalProviderFailed) || answer != "" {
		t.Fatalf("verdict=%s refusal=%q answer=%q", got.Verdict, got.Refusal, answer)
	}
	if len(got.Diagnostics) == 0 || got.Diagnostics[0] != want {
		t.Fatalf("diagnostics=%+v want first %+v", got.Diagnostics, want)
	}
	public, err := json.Marshal(got.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{quotaProviderCanary, "1793171400", "five_hour", "out_of_credits", "429", "rejected", "overage"} {
		if bytes.Contains(public, []byte(leaked)) {
			t.Fatalf("diagnostics leak %q: %s", leaked, public)
		}
	}
}

func genericProviderFailure() task.OutputDiagnostic {
	return claudeCurrentDiagnostic(refusalProviderFailed)
}

func TestFollowUpPolicyReportsCorrelatedQuotaExhaustion(t *testing.T) {
	initial, principal := writeInitLine(claudeTestUUID), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1")
	for name, lines := range map[string][]string{
		"sealed principal shape": {initial, rateLimitLine("allowed_warning"), rateLimitLine("rejected"), apiErrorResult(`,"result_index":0`)},
		"unindexed principal":    {initial, rateLimitLine("rejected"), apiErrorResult("")},
		"follow-up cycle": {
			initial, principal, initial, interactiveAssistant("checking"), rateLimitLine("rejected"), apiErrorResult(notificationAttribution(1)),
		},
		"leading acknowledgement": {initial, rateLimitLine("rejected"), mutateLine(apiErrorResult(notificationAttribution(0)), `"num_turns":1`, `"num_turns":0`)},
	} {
		for _, exitCode := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit %d", name, exitCode), func(t *testing.T) {
				got, answer := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), lines, exitCode)
				requireQuotaRejection(t, got, answer, claudeQuotaDiagnostic())
			})
		}
	}
}

func TestFollowUpPolicyKeepsUncorrelatedQuotaGeneric(t *testing.T) {
	initial, principal := writeInitLine(claudeTestUUID), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1")
	failed := apiErrorResult(`,"result_index":0`)
	maxTurns := mutateLine(failed, `"terminal_reason":"api_error"`, `"terminal_reason":"max_turns"`)
	for name, lines := range map[string][]string{
		"no rate-limit event":            {initial, failed},
		"warning only":                   {initial, rateLimitLine("allowed_warning"), failed},
		"warning after rejection":        {initial, rateLimitLine("rejected"), rateLimitLine("allowed_warning"), failed},
		"unknown status":                 {initial, rateLimitLine("throttled"), failed},
		"case-variant status":            {initial, rateLimitLine("Rejected"), failed},
		"string info":                    {initial, rateLimitInfoLine(`"rejected"`), failed},
		"null info":                      {initial, rateLimitInfoLine(`null`), failed},
		"numeric status":                 {initial, rateLimitInfoLine(`{"status":1}`), failed},
		"missing status":                 {initial, rateLimitInfoLine(`{"rateLimitType":"five_hour"}`), failed},
		"aliased status":                 {initial, rateLimitInfoLine(`{"status":"rejected","Status":"rejected"}`), failed},
		"malformed after rejection":      {initial, rateLimitLine("rejected"), rateLimitInfoLine(`[]`), failed},
		"missing session":                {initial, strings.Replace(rateLimitLine("rejected"), fmt.Sprintf(`,"session_id":%q`, claudeTestUUID), "", 1), failed},
		"rejection before init":          {rateLimitLine("rejected"), initial, failed},
		"other failure reason":           {initial, rateLimitLine("rejected"), maxTurns},
		"stale rejection settled before": {initial, rateLimitLine("rejected"), principal, initial, interactiveAssistant("checking"), apiErrorResult(notificationAttribution(1))},
	} {
		for _, exitCode := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit %d", name, exitCode), func(t *testing.T) {
				got, answer := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), lines, exitCode)
				requireQuotaRejection(t, got, answer, genericProviderFailure())
			})
		}
	}
}

func TestQuotaEventsAloneNeverReject(t *testing.T) {
	initial, principal := writeInitLine(claudeTestUUID), attributedResultLine(tracePrincipalText, `,"result_index":0`, 1, "0.1")
	for _, event := range []string{rateLimitLine("allowed_warning"), rateLimitLine("rejected"), rateLimitInfoLine(`{"status":"rejected","Status":"x"}`), rateLimitInfoLine(`7`)} {
		lines := []string{initial, event, principal}
		got, answer := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), lines, 0)
		if got.Verdict != task.VerdictCommitted || answer != tracePrincipalText || len(got.Diagnostics) != 0 {
			t.Fatalf("%s: %+v answer=%q", event, got, answer)
		}
	}
}

func TestQuotaDiagnosticRequiresTrustedStreamAndIdentity(t *testing.T) {
	lines := []string{writeInitLine(claudeTestUUID), rateLimitLine("rejected"), apiErrorResult(`,"result_index":0`)}
	stdout := jsonl(lines...)
	v := NewWorkspaceWriteInterpreter()
	other := "123e4567-e89b-12d3-a456-426614174001"
	for name, mutate := range map[string]func(*predicate.Input){
		"seal error":        func(input *predicate.Input) { input.Seal.Error = "capture failed" },
		"identity mismatch": func(input *predicate.Input) { input.ExpectedSession.ID = other },
	} {
		input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
		input.Seal.ExitCode = 1
		mutate(&input)
		got, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Verdict != task.VerdictRejected || len(got.Diagnostics) == 0 || got.Diagnostics[0].Code == claudeQuotaDiagnostic().Code {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	wrongSession := strings.Replace(rateLimitLine("rejected"), claudeTestUUID, other, 1)
	got, _ := evaluateQuotaTrace(t, v, []string{lines[0], wrongSession, lines[2]}, 1)
	if got.Verdict != task.VerdictRejected || len(got.Diagnostics) == 0 || got.Diagnostics[0].Code == claudeQuotaDiagnostic().Code {
		t.Fatalf("wrong session: %+v", got)
	}
}

func TestFrozenPoliciesKeepGenericQuotaDiagnostics(t *testing.T) {
	lines := []string{writeInitLine(claudeTestUUID), rateLimitLine("rejected"), apiErrorResult(`,"result_index":0`)}
	for _, v := range []predicate.Interpreter{newAdvisoryInterpreter(WorkspaceWriteMode), newScopedInterpreter(WorkspaceWriteMode)} {
		// The frozen wire keeps a successful result with is_error true
		// malformed; only the seal's exit code makes it a provider failure.
		got, answer := evaluateQuotaTrace(t, v, lines, 0)
		if got.Verdict != task.VerdictRejected || got.Refusal != refusalMalformed || answer != "" || len(got.Diagnostics) == 0 ||
			got.Diagnostics[0] != claudeCurrentDiagnostic(refusalMalformed) {
			t.Fatalf("%+v: %+v answer=%q", v.Reference(), got, answer)
		}
		got, answer = evaluateQuotaTrace(t, v, lines, 1)
		requireQuotaRejection(t, got, answer, genericProviderFailure())
	}
}
