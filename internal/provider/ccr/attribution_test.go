package ccr

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const ccrAttributionSession = "123e4567-e89b-12d3-a456-426614174000"

// ccrFollowUpTrace is a neutral Claude stream with one caller result and one
// provider-native background notification cycle after it.
func ccrFollowUpTrace() string {
	init := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read","Glob","Grep"],"mcp_servers":[],"model":"fixture","permissionMode":"plan"}`, ccrAttributionSession)
	result := `{"type":"result","subtype":"success","is_error":false,"result":%q,"stop_reason":"end_turn","session_id":%q,"total_cost_usd":%s,"num_turns":1%s}`
	return strings.Join([]string{
		init,
		fmt.Sprintf(result, "principal answer", ccrAttributionSession, "0.1", `,"result_index":0`),
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","session_id":%q,"task_id":"bg-1","status":"completed"}`, ccrAttributionSession),
		init,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral"}]}}`, ccrAttributionSession),
		fmt.Sprintf(result, "neutral", ccrAttributionSession, "0.2", `,"result_index":1,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-1"}`),
	}, "\n")
}

// ccrNestedMessageTrace is a neutral Claude stream whose principal result is
// followed by nested agent messages (a non-null parent_tool_use_id) before the
// provider-native background notification cycle repeats init.
func ccrNestedMessageTrace(mode, nestedParent string) string {
	permission := "plan"
	if mode == config.ModeWorkspaceWrite {
		permission = "bypassPermissions"
	}
	init := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read","Glob","Grep"],"mcp_servers":[],"model":"fixture","permissionMode":%q}`, ccrAttributionSession, permission)
	result := `{"type":"result","subtype":"success","is_error":false,"result":%q,"stop_reason":"end_turn","session_id":%q,"total_cost_usd":%s,"num_turns":1%s}`
	message := `{"type":%q,"session_id":%q%s,"message":{"content":[{"type":"text","text":"neutral"}]}}`
	return strings.Join([]string{
		init,
		fmt.Sprintf(result, "principal answer", ccrAttributionSession, "0.1", `,"result_index":0`),
		fmt.Sprintf(message, "assistant", ccrAttributionSession, nestedParent),
		fmt.Sprintf(message, "user", ccrAttributionSession, nestedParent),
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","session_id":%q,"task_id":"bg-1","status":"completed"}`, ccrAttributionSession),
		init,
		fmt.Sprintf(message, "assistant", ccrAttributionSession, `,"parent_tool_use_id":null`),
		fmt.Sprintf(result, "neutral", ccrAttributionSession, "0.2", `,"result_index":1,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-1"}`),
	}, "\n")
}

func evaluateCCRTrace(t *testing.T, interpreter predicate.Interpreter, stdout string) (task.Interpretation, string) {
	t.Helper()
	input, raw := credentialInput(t, interpreter.Reference(), 0, stdout, "")
	input.ExpectedSession = task.SessionExpectation{Required: true, ID: ccrAttributionSession}
	var answer bytes.Buffer
	result, err := interpreter.Evaluate(input, raw, &answer)
	if err != nil {
		t.Fatal(err)
	}
	return result, answer.String()
}

func TestCurrentCCRReferencesPublishPrincipalAnswerAfterNativeFollowUp(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []task.PredicateRef{Reference(config.ModeReadOnly), modeledReference(config.ModeReadOnly)} {
		interpreter, resolveErr := registry.Resolve(reference)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		result, answer := evaluateCCRTrace(t, interpreter, ccrFollowUpTrace())
		if result.Verdict != task.VerdictCommitted || answer != "principal answer" || result.Session == nil || result.Session.Provider != Provider {
			t.Fatalf("%s interpretation=%+v answer=%q", reference.Version, result, answer)
		}
	}
	for _, reference := range []task.PredicateRef{preAttributionReference(config.ModeReadOnly, true), preAttributionReference(config.ModeReadOnly, false)} {
		interpreter, resolveErr := registry.Resolve(reference)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		result, answer := evaluateCCRTrace(t, interpreter, ccrFollowUpTrace())
		if result.Verdict != task.VerdictRejected || result.Refusal != "invalid-output" || answer != "" {
			t.Fatalf("%s historical replay changed: %+v answer=%q", reference.Version, result, answer)
		}
	}
}

func TestPreAttributionCCRSealRecoversToAttributedTargetWithoutRewriting(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	source := preAttributionReference(config.ModeReadOnly, true)
	recovery, err := registry.ResolveRecovery(source)
	if err != nil || !recovery.Target().Equal(Reference(config.ModeReadOnly)) {
		t.Fatalf("recovery=%v err=%v", recovery, err)
	}
	input, raw := credentialInput(t, source, 0, ccrFollowUpTrace(), "")
	input.ExpectedSession = task.SessionExpectation{Required: true, ID: ccrAttributionSession}
	var answer bytes.Buffer
	result, err := recovery.Recover(input, raw, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != "principal answer" || !input.Seal.Predicate.Equal(source) {
		t.Fatalf("recovery=%+v answer=%q err=%v seal=%+v", result, answer.String(), err, input.Seal.Predicate)
	}
}

func TestExistingPreAttributionCCRProfilesRetainTheirLaunchContract(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "ccr"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", directory)
	t.Setenv("PATH", directory)
	request := testRequest()
	request.CanonicalCwd = directory
	for _, test := range []struct {
		reference   task.PredicateRef
		resumeRoute bool
	}{
		{preAttributionReference(request.Mode, true), true},
		{preAttributionReference(request.Mode, false), false},
	} {
		existing, prepareErr := PrepareExistingCandidate(request, task.MetaRecord{Predicate: test.reference, Environment: []string{"HOME=" + directory, "PATH=" + directory}, EnvironmentRecorded: true}, os.Environ())
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if _, finalErr := existing.Finalize(routeFacts(t, existing, !test.resumeRoute), time.Now()); !errors.Is(finalErr, commonprovider.ErrProfileUnavailable) {
			t.Fatalf("%s accepted the other route proof: %v", test.reference.Version, finalErr)
		}
		rebuilt, finalErr := existing.Finalize(routeFacts(t, existing, test.resumeRoute), time.Now())
		if finalErr != nil || !rebuilt.Plan.Predicate.Equal(test.reference) {
			t.Fatalf("%s reconstruction changed predicate=%+v err=%v", test.reference.Version, rebuilt.Plan.Predicate, finalErr)
		}
	}
	predecessor := testRequest()
	successor := predecessor
	successor.RequestedConfig.Model = "other-model"
	if err := validateContinuation(predecessor, task.MetaRecord{Predicate: preAttributionReference(predecessor.Mode, true)}, successor); !errors.Is(err, commonprovider.ErrUnsupportedOption) {
		t.Fatalf("route-preserving v4 continuation accepted a model change: %v", err)
	}
}

func TestCurrentCCRReferencesRouteNestedAgentMessagesByScope(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, reference := range []task.PredicateRef{Reference(mode), modeledReference(mode)} {
			requireNestedScopeRouting(t, registry, mode, reference, true)
		}
		for _, reference := range []task.PredicateRef{preAttributionReference(mode, true), preAttributionReference(mode, false)} {
			requireNestedScopeRouting(t, registry, mode, reference, false)
		}
	}
}

// requireNestedScopeRouting evaluates a reference against nested agent
// messages. Scoped current references publish the principal and reject any
// invalid scope; frozen single-result references keep rejecting the stream.
func requireNestedScopeRouting(t *testing.T, registry predicate.Registry, mode string, reference task.PredicateRef, scoped bool) {
	t.Helper()
	interpreter, err := registry.Resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	result, answer := evaluateCCRTrace(t, interpreter, ccrNestedMessageTrace(mode, `,"parent_tool_use_id":"toolu_nested_agent"`))
	if !scoped {
		if result.Verdict != task.VerdictRejected {
			t.Fatalf("%s/%s historical replay changed: %+v", mode, reference.Version, result)
		}
		return
	}
	if result.Verdict != task.VerdictCommitted || answer != "principal answer" {
		t.Fatalf("%s/%s nested messages interpretation=%+v answer=%q", mode, reference.Version, result, answer)
	}
	for _, invalid := range []string{`,"parent_tool_use_id":""`, `,"parent_tool_use_id":7`, `,"parent_tool_use_id":{}`} {
		result, answer = evaluateCCRTrace(t, interpreter, ccrNestedMessageTrace(mode, invalid))
		if result.Verdict != task.VerdictRejected || answer != "" {
			t.Fatalf("%s/%s accepted %s: %+v answer=%q", mode, reference.Version, invalid, result, answer)
		}
	}
}

// ccrLeadingAcknowledgementTrace is a resumed-session stream: an empty
// zero-turn background acknowledgement precedes the caller's own result.
func ccrLeadingAcknowledgementTrace(mode, acknowledgement string) string {
	permission := "plan"
	if mode == config.ModeWorkspaceWrite {
		permission = "bypassPermissions"
	}
	init := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"claude_code_version":"fixture","apiKeySource":"none","cwd":"/workspace","tools":["Read","Glob","Grep"],"mcp_servers":[],"model":"fixture","permissionMode":%q}`, ccrAttributionSession, permission)
	result := `{"type":"result","subtype":"success","is_error":false,"result":%q,"stop_reason":"end_turn","session_id":%q,"total_cost_usd":%s,"num_turns":%d%s}`
	return strings.Join([]string{
		fmt.Sprintf(`{"type":"system","subtype":"task_notification","session_id":%q,"task_id":"bg-old","status":"stopped"}`, ccrAttributionSession),
		init,
		acknowledgement,
		init,
		fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"text","text":"neutral"}]}}`, ccrAttributionSession),
		fmt.Sprintf(result, "principal answer", ccrAttributionSession, "0.35", 1, `,"result_index":1`),
	}, "\n")
}

func ccrAcknowledgement(extra string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"result":"","session_id":%q,"total_cost_usd":0.05,"num_turns":0,"result_index":0,"origin":{"kind":"task-notification","producer":"session-task","runId":"run-0"}%s}`, ccrAttributionSession, extra)
}

func TestCurrentCCRReferencesAcceptLeadingAcknowledgements(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for _, reference := range []task.PredicateRef{Reference(mode), modeledReference(mode)} {
			requireLeadingAcknowledgementRouting(t, registry, mode, reference)
		}
		for _, reference := range []task.PredicateRef{preAttributionReference(mode, true), preAttributionReference(mode, false)} {
			interpreter, resolveErr := registry.Resolve(reference)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if result, _ := evaluateCCRTrace(t, interpreter, ccrLeadingAcknowledgementTrace(mode, ccrAcknowledgement(""))); result.Verdict != task.VerdictRejected {
				t.Fatalf("%s/%s historical replay changed: %+v", mode, reference.Version, result)
			}
		}
	}
}

// requireLeadingAcknowledgementRouting evaluates a current reference: a plain
// acknowledgement publishes the caller's answer, a failed one keeps its
// canonical refusal, and a nonempty one is malformed.
func requireLeadingAcknowledgementRouting(t *testing.T, registry predicate.Registry, mode string, reference task.PredicateRef) {
	t.Helper()
	interpreter, err := registry.Resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	result, answer := evaluateCCRTrace(t, interpreter, ccrLeadingAcknowledgementTrace(mode, ccrAcknowledgement("")))
	if result.Verdict != task.VerdictCommitted || answer != "principal answer" {
		t.Fatalf("%s/%s leading acknowledgement interpretation=%+v answer=%q", mode, reference.Version, result, answer)
	}
	failed := ccrAcknowledgement(`,"terminal_reason":"aborted_tools"`)
	if result, answer = evaluateCCRTrace(t, interpreter, ccrLeadingAcknowledgementTrace(mode, failed)); result.Verdict != task.VerdictRejected || answer != "" ||
		result.Refusal != "provider-failed: terminal_reason=aborted_tools" {
		t.Fatalf("%s/%s failed acknowledgement was not retained: %+v answer=%q", mode, reference.Version, result, answer)
	}
	nonempty := strings.Replace(ccrAcknowledgement(""), `"result":""`, `"result":"x"`, 1)
	if result, _ = evaluateCCRTrace(t, interpreter, ccrLeadingAcknowledgementTrace(mode, nonempty)); result.Verdict != task.VerdictRejected || result.Refusal != "invalid-output" {
		t.Fatalf("%s/%s accepted a nonempty acknowledgement: %+v", mode, reference.Version, result)
	}
}
