package ccr

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const ccrFollowUpPayload = "[delegate responses v1]\n[delegate response result_index=0 origin=caller bytes=16]\nprincipal answer\n[delegate response result_index=1 origin=task-notification bytes=7]\nneutral\n"

func TestCCRFollowUpPolicyPreservesAdvisoryReplay(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, route := range []bool{true, false} {
			requireCCRProjectionReplay(t, registry, mode, route)
		}
	}
}

func requireCCRProjectionReplay(t *testing.T, registry predicate.Registry, mode string, route bool) {
	t.Helper()
	trace := ccrFollowUpTrace()
	if mode == config.ModeWorkspaceWrite {
		trace = strings.ReplaceAll(trace, `"permissionMode":"plan"`, `"permissionMode":"bypassPermissions"`)
	}
	frozen, err := registry.Resolve(advisoryReference(mode, route))
	if err != nil {
		t.Fatal(err)
	}
	got, answer := evaluateCCRTrace(t, frozen, trace)
	if got.Verdict != task.VerdictCommitted || answer != "principal answer" {
		t.Fatalf("%+v: %+v %q", frozen.Reference(), got, answer)
	}
	current := Reference(mode)
	if !route {
		current = modeledReference(mode)
	}
	v, err := registry.Resolve(current)
	if err != nil {
		t.Fatal(err)
	}
	got, answer = evaluateCCRTrace(t, v, trace)
	if got.Verdict != task.VerdictCommitted || answer != ccrFollowUpPayload {
		t.Fatalf("%+v: %+v %q", current, got, answer)
	}
	single := strings.Join(strings.Split(trace, "\n")[:2], "\n")
	got, answer = evaluateCCRTrace(t, v, single)
	if got.Verdict != task.VerdictCommitted || answer != "principal answer" {
		t.Fatalf("%+v single: %+v %q", current, got, answer)
	}
}

// ccrQuotaTrace is the sealed quota failure shape: the latest same-session
// rate-limit event is a structured rejection and the root result is a failed
// final request with terminal_reason api_error.
func ccrQuotaTrace() string {
	lines := strings.Split(ccrFollowUpTrace(), "\n")
	rateLimit := fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1793171400,"overageStatus":"rejected","overageDisabledReason":"out_of_credits"},"session_id":%q}`, ccrAttributionSession)
	failed := fmt.Sprintf(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429","stop_reason":"stop_sequence","terminal_reason":"api_error","session_id":%q,"num_turns":1,"result_index":0}`, ccrAttributionSession)
	return strings.Join([]string{lines[0], rateLimit, failed}, "\n")
}

func TestCCRFollowUpPolicyReportsQuotaWhileFrozenReplayStaysGeneric(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	quota := task.OutputDiagnostic{Code: "provider_quota_exhausted", Message: "Claude reported that its usage quota was exhausted.", Field: "rate_limit_info"}
	for _, test := range []struct {
		reference task.PredicateRef
		want      string
	}{
		{Reference(config.ModeReadOnly), quota.Code},
		{modeledReference(config.ModeReadOnly), quota.Code},
		{advisoryReference(config.ModeReadOnly, true), "provider_failed"},
		{advisoryReference(config.ModeReadOnly, false), "provider_failed"},
	} {
		interpreter, resolveErr := registry.Resolve(test.reference)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		input, raw := credentialInput(t, interpreter.Reference(), 1, ccrQuotaTrace(), "")
		input.ExpectedSession = task.SessionExpectation{Required: true, ID: ccrAttributionSession}
		var answer bytes.Buffer
		got, evalErr := interpreter.Evaluate(input, raw, &answer)
		if evalErr != nil || got.Verdict != task.VerdictRejected || answer.Len() != 0 || len(got.Diagnostics) == 0 || got.Diagnostics[0].Code != test.want ||
			(test.want == quota.Code && got.Diagnostics[0] != quota) {
			t.Fatalf("%s: %+v answer=%q err=%v", test.reference.Version, got, answer.String(), evalErr)
		}
	}
}
