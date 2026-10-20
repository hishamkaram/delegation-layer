package ccr

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ccrAdvisoryTrace is ccrFollowUpTrace with advisory-only init telemetry: the
// repeated init drops every advisory field and repeats controls that only
// other system subtypes consume, and an earlier assistant message carries an
// unknown content block with opaque repeated keys.
func ccrAdvisoryTrace() string {
	lines := strings.Split(ccrFollowUpTrace(), "\n")
	lines[3] = fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q,"permissionMode":"plan","future":{"a":1,"A":2},"state":1,"state":2,"task_id":"a","Task_Id":"b"}`, ccrAttributionSession)
	opaque := fmt.Sprintf(`{"type":"assistant","session_id":%q,"message":{"content":[{"type":"server_tool_use_v9","input":{"a":1,"A":2}}]}}`, ccrAttributionSession)
	return strings.Join(append([]string{lines[0], opaque}, lines[1:]...), "\n")
}

// TestCurrentCCRReferencesBindTheAdvisoryPolicy evaluates the same advisory
// trace through every CCR revision: the current v6 references publish the
// principal answer, while the frozen v5 references keep their strict wire.
func TestCurrentCCRReferencesBindTheAdvisoryPolicy(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		reference task.PredicateRef
		commit    bool
	}{
		{Reference(config.ModeReadOnly), true},
		{modeledReference(config.ModeReadOnly), true},
		{scopedReference(config.ModeReadOnly, true), false},
		{scopedReference(config.ModeReadOnly, false), false},
	} {
		interpreter, resolveErr := registry.Resolve(test.reference)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		result, answer := evaluateCCRTrace(t, interpreter, ccrAdvisoryTrace())
		if test.commit != (result.Verdict == task.VerdictCommitted && answer == "principal answer") ||
			(!test.commit && (result.Refusal != "invalid-output" || answer != "")) {
			t.Fatalf("%s interpretation=%+v answer=%q", test.reference.Version, result, answer)
		}
		if test.commit && (result.Session == nil || *result.Session != (task.SessionIdentity{Provider: Provider, ConversationID: ccrAttributionSession})) {
			t.Fatalf("%s session=%+v", test.reference.Version, result.Session)
		}
	}
}

func TestCurrentCCRReferencesReportUnsupportedCompletion(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	const canary = "sk-ant-canary-ccr-secret"
	trace := strings.Replace(ccrFollowUpTrace(), `"stop_reason":"end_turn"`, `"stop_reason":"`+canary+`"`, 1)
	for reference, want := range map[task.PredicateRef]string{
		Reference(config.ModeReadOnly):              "unsupported-completion",
		scopedReference(config.ModeReadOnly, true):  "invalid-output",
		modeledReference(config.ModeReadOnly):       "unsupported-completion",
		scopedReference(config.ModeReadOnly, false): "invalid-output",
	} {
		interpreter, resolveErr := registry.Resolve(reference)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		result, answer := evaluateCCRTrace(t, interpreter, trace)
		if result.Verdict != task.VerdictRejected || result.Refusal != want || answer != "" || strings.Contains(fmt.Sprintf("%+v", result), canary) {
			t.Fatalf("%s interpretation=%+v answer=%q", reference.Version, result, answer)
		}
		if want == "unsupported-completion" && (len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != "unsupported_completion" || result.Diagnostics[0].Field != "stop_reason") {
			t.Fatalf("%s diagnostics=%+v", reference.Version, result.Diagnostics)
		}
	}
}

func TestScopedCCRSealsRecoverToCurrentTargetsWithoutRewriting(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for route, target := range map[bool]task.PredicateRef{true: Reference(config.ModeReadOnly), false: modeledReference(config.ModeReadOnly)} {
		source := scopedReference(config.ModeReadOnly, route)
		recovery, resolveErr := registry.ResolveRecovery(source)
		if resolveErr != nil || !recovery.Target().Equal(target) {
			t.Fatalf("%s recovery=%v err=%v", source.Version, recovery, resolveErr)
		}
		input, raw := credentialInput(t, source, 0, ccrAdvisoryTrace(), "")
		input.ExpectedSession = task.SessionExpectation{Required: true, ID: ccrAttributionSession}
		var answer strings.Builder
		result, recoverErr := recovery.Recover(input, raw, &answer)
		if recoverErr != nil || result.Verdict != task.VerdictCommitted || answer.String() != "principal answer" || !input.Seal.Predicate.Equal(source) {
			t.Fatalf("%s recovery=%+v answer=%q err=%v", source.Version, result, answer.String(), recoverErr)
		}
		frozen, resolveErr := registry.Resolve(source)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		replayOnly, ok := frozen.(predicate.RecoveryInterpreter)
		if !ok {
			t.Fatal("scoped CCR interpreter lost its recovery contract")
		}
		if _, recoverErr = replayOnly.Recover(input, raw, &answer); !errors.Is(recoverErr, task.ErrIncompatiblePredicate) {
			t.Fatalf("%s scoped reference became a recovery target: %v", source.Version, recoverErr)
		}
	}
}

func TestScopedCCRReferencesKeepTheirLaunchContract(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		for route, launch := range map[bool]preparationRevision{true: resumeRoutePreparation, false: legacyPreparation} {
			contract, err := preparationForReference(scopedReference(mode, route), mode)
			if err != nil || contract.launch != launch || contract.inspection != routeInspection {
				t.Fatalf("%s route=%v preparation=%+v err=%v", mode, route, contract, err)
			}
		}
	}
	predecessor := testRequest()
	successor := predecessor
	successor.RequestedConfig.Model = "other-model"
	for _, reference := range []task.PredicateRef{scopedReference(predecessor.Mode, true), Reference(predecessor.Mode)} {
		if err := validateContinuation(predecessor, task.MetaRecord{Predicate: reference}, successor); err == nil {
			t.Fatalf("route-preserving %s continuation accepted a model change", reference.Version)
		}
	}
}
