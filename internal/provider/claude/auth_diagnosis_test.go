package claude

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func authenticationAssistant(scope string) string {
	return fmt.Sprintf(`{"type":"assistant","session_id":%q,"parent_tool_use_id":%s,"error":"authentication_failed","message":{"role":"assistant","content":[{"type":"text","text":"private auth text"}]}}`, claudeTestUUID, scope)
}

func TestAuthenticationDiagnosisRequiresStructuredRootFailure(t *testing.T) {
	for name, assistant := range map[string]string{"root": authenticationAssistant("null"), "nested": authenticationAssistant(`"tool-1"`), "unknown": mutateLine(authenticationAssistant("null"), `"authentication_failed"`, `"unknown"`), "text only": interactiveAssistant("authentication_failed")} {
		t.Run(name, func(t *testing.T) {
			got, answer := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), []string{writeInitLine(claudeTestUUID), assistant, apiErrorResult(`,"result_index":0`)}, 1)
			if got.Verdict != task.VerdictRejected || answer != "" {
				t.Fatalf("failure published: %+v", got)
			}
			want := "provider_failed"
			if name == "root" {
				want = "provider_authentication_failed"
			}
			if len(got.Diagnostics) == 0 || got.Diagnostics[0].Code != want {
				t.Fatalf("diagnostics=%+v want %s", got.Diagnostics, want)
			}
		})
	}
}

func TestAuthenticationDiagnosisKeepsUnvalidatedEvidenceGeneric(t *testing.T) {
	initial := writeInitLine(claudeTestUUID)
	assistant := authenticationAssistant("null")
	failed := apiErrorResult(`,"result_index":0`)
	for name, lines := range map[string][]string{
		"foreign session":      {initial, mutateLine(assistant, claudeTestUUID, "11111111-1111-4111-8111-111111111111"), failed},
		"before init":          {assistant, initial, failed},
		"ambiguous error":      {initial, mutateLine(assistant, `"error":"authentication_failed"`, `"error":"authentication_failed","ERROR":"authentication_failed"`), failed},
		"successful result":    {initial, assistant, attributedResultLine("answer", `,"result_index":0`, 1, "0.1")},
		"other failure":        {initial, assistant, mutateLine(failed, `"terminal_reason":"api_error"`, `"terminal_reason":"max_turns"`)},
		"consumed correlation": {initial, assistant, attributedResultLine("answer", `,"result_index":0`, 1, "0.1"), apiErrorResult(notificationAttribution(1))},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), lines, 1)
			if task.HasProviderAuthenticationFailure(got.Diagnostics) {
				t.Fatalf("unvalidated auth diagnosis: %+v", got)
			}
		})
	}
}

func TestAuthenticationDiagnosisRetainsPriorPolicyAndUsageWarning(t *testing.T) {
	lines := []string{writeInitLine(claudeTestUUID), authenticationAssistant("null"), mutateLine(apiErrorResult(`,"result_index":0`), `"usage":{`, `"usage":{"input_tokens":"invalid",`)}
	// A malformed but present usage counter is advisory even on rejected output.
	for _, v := range []predicate.Interpreter{NewWorkspaceWriteInterpreter(), newFollowUpInterpreter(WorkspaceWriteMode)} {
		got, _ := evaluateQuotaTrace(t, v, lines, 1)
		wantAuth := v.Reference().Equal(WorkspaceWriteReference())
		if task.HasProviderAuthenticationFailure(got.Diagnostics) != wantAuth {
			t.Fatalf("revision classification=%+v diagnostics=%+v", v.Reference(), got.Diagnostics)
		}
		warning := false
		for _, d := range got.Diagnostics {
			warning = warning || d.Code == "optional_usage_unavailable"
		}
		if !warning {
			t.Fatalf("present unreliable usage warning lost: %+v", got.Diagnostics)
		}
	}
}

func TestAuthenticationDiagnosisInteractiveAndOperationalPrecedence(t *testing.T) {
	lines := []string{writeInitLine(claudeTestUUID), authenticationAssistant("null"), apiErrorResult(`,"result_index":0`)}
	got, _ := evaluateInteractiveTrace(t, newInteractionInterpreter(WorkspaceWriteMode), wrapInteractiveSession(lines))
	if !task.HasProviderAuthenticationFailure(got.Diagnostics) {
		t.Fatalf("interactive auth=%+v", got)
	}
	frozen, _ := evaluateInteractiveTrace(t, newFollowUpInteractionInterpreter(WorkspaceWriteMode), wrapInteractiveSession(lines))
	if task.HasProviderAuthenticationFailure(frozen.Diagnostics) {
		t.Fatalf("frozen revision changed: %+v", frozen)
	}
	stdout := jsonl(lines...)
	v := NewWorkspaceWriteInterpreter()
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: task.SessionExpectation{Required: true, ID: claudeTestUUID}}
	input.Seal.Error = "capture failed"
	var answer bytes.Buffer
	got, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatal(err)
	}
	if task.HasProviderAuthenticationFailure(got.Diagnostics) {
		t.Fatalf("capture fault mislabeled: %+v", got)
	}
}

func TestAuthenticationRevisionKeepsRetainedReferences(t *testing.T) {
	registry := retainedRegistry(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		current := ReferenceForMode(mode)
		wantVersion := "native-permissions-v7"
		if mode == WorkspaceWriteMode {
			wantVersion = "native-permissions-v8"
		}
		if current.Version != wantVersion || interactionReference(mode).Version != "interactive-v8" {
			t.Fatalf("fresh revisions=%+v %+v", current, interactionReference(mode))
		}
		for _, ref := range []task.PredicateRef{followUpReference(mode), followUpInteractionReference(mode)} {
			requireRetainedAuthenticationReference(t, registry, ref)
		}
		if !strings.Contains(ContractForMode(mode), authenticationPolicy) || !strings.Contains(authenticationInteractionContract(), authenticationPolicy) {
			t.Fatal("fresh contract omits auth policy")
		}
	}
}

func requireRetainedAuthenticationReference(t *testing.T, registry predicate.Registry, ref task.PredicateRef) {
	t.Helper()
	resolved, err := registry.Resolve(ref)
	if err != nil || !resolved.Reference().Equal(ref) {
		t.Fatalf("retained reference missing: %+v %v", ref, err)
	}
	request := task.TaskRecord{Mode: ref.Mode}
	request.RequestedConfig.Interactive = ref.Version == "interactive-v7"
	options, err := normalizeNativePreparationOptions(request, nativePreparationOptions{predicateReference: ref})
	if err != nil || !options.predicateReference.Equal(ref) || options.permissionMode != nativeDefaultPermission(ref.Mode) {
		t.Fatalf("retained reconstruction failed: %+v %v", options, err)
	}
	if _, err := registry.ResolveRecovery(ref); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
		t.Fatalf("diagnostic-only revision became recovery source: %v", err)
	}
}
