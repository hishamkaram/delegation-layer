package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The current advisory revisions use version strings no earlier digest of the
// same mode uses; their digests are pinned so a contract edit that silently
// stops matching its frozen clause fails here.
const (
	advisoryReadDigest             = "a4f38219dcb1a105cf20c75f54341effde2310b6a15f61665a3b597eaa205fa8"
	advisoryWriteDigest            = "7b64659e74a5b11e4717b5f942efa577544c2dd0730479e0f47a05ff0402bd8b"
	advisoryReadInteractiveDigest  = "f06decd5719453bb8282dcb464befef53e0511e63b05ca71cf78b32bc622876c"
	advisoryWriteInteractiveDigest = "2e7aeed42a66ad48127010a0591187a83a39c58a02973b1037542052020b2b95"
	scopedReadInteractiveDigest    = "9a104f6c0941b9604ce04a5fe209699de17a3e1ac659e30a58b99ec32939b7d0"
	scopedWriteInteractiveDigest   = "bd63d2e508a83af6e92ccd98dee85eded7ab2ca484d44d388af3b66bc405a2d5"
	unsupportedReasonCanary        = "sk-ant-canary-0000-secret"
)

func TestAdvisoryRevisionsPinIdentityAndEveryContractEdit(t *testing.T) {
	for _, revision := range []struct {
		reference, scoped task.PredicateRef
		version, digest   string
		scopedDigest      string
	}{
		{ReferenceForMode(Mode), scopedReference(Mode), "native-permissions-v5", advisoryReadDigest, scopedReadDigest},
		{ReferenceForMode(WorkspaceWriteMode), scopedReference(WorkspaceWriteMode), "native-permissions-v6", advisoryWriteDigest, scopedWriteDigest},
		{interactionReference(Mode), scopedInteractionReference(Mode), "interactive-v6", advisoryReadInteractiveDigest, scopedReadInteractiveDigest},
		{interactionReference(WorkspaceWriteMode), scopedInteractionReference(WorkspaceWriteMode), "interactive-v6", advisoryWriteInteractiveDigest, scopedWriteInteractiveDigest},
	} {
		if revision.reference.Version != revision.version || revision.reference.SHA256 != revision.digest || revision.scoped.SHA256 != revision.scopedDigest {
			t.Fatalf("revision identity changed: current=%+v scoped=%+v", revision.reference, revision.scoped)
		}
	}
	for mode, edits := range map[string][]contractEdit{
		Mode:               append(advisoryReadOnlyEdits(), advisoryCompletionEdits()...),
		WorkspaceWriteMode: append(advisoryWorkspaceWriteEdits(), advisoryCompletionEdits()...),
	} {
		requireContractEdits(t, mode, scopedNativeContract(mode), ContractForMode(mode), edits)
	}
	requireContractEdits(t, "interactive", scopedInteractionContract, interactionContract(), advisoryInteractionEdits())
}

// requireContractEdits requires every edit to match its frozen clause exactly
// once and to appear in the derived contract.
func requireContractEdits(t *testing.T, label, frozen, derived string, edits []contractEdit) {
	t.Helper()
	if frozen == derived {
		t.Fatalf("%s derived contract equals its frozen bytes", label)
	}
	for _, edit := range edits {
		if strings.Count(frozen, edit.old) != 1 || !strings.Contains(derived, edit.new) {
			t.Fatalf("%s edit does not apply exactly once: %q", label, edit.old)
		}
	}
}

func unsupportedCompletionLines(principal string) []string {
	return []string{interactiveInitLine(WorkspaceWriteMode), principal}
}

func TestCurrentUnknownCompletionReasonIsUnsupported(t *testing.T) {
	principal := principalLine()
	stop := func(value string) string {
		return mutateLine(principal, `"stop_reason":"end_turn"`, `"stop_reason":`+value)
	}
	terminal := func(value string) string {
		return mutateLine(principal, `"terminal_reason":"completed"`, `"terminal_reason":`+value)
	}
	canary := `"` + unsupportedReasonCanary + `"`
	oversized := `"` + strings.Repeat("r", 129) + `"`
	for _, tc := range []struct {
		name, line, want, field string
	}{
		{"unknown stop_reason", stop(canary), refusalUnsupportedCompletion, "stop_reason"},
		{"empty stop_reason", stop(`""`), refusalUnsupportedCompletion, "stop_reason"},
		{"unknown terminal_reason", terminal(canary), refusalUnsupportedCompletion, "terminal_reason"},
		{"unknown stop and terminal", mutateLine(stop(canary), `"terminal_reason":"completed"`, `"terminal_reason":`+canary), refusalUnsupportedCompletion, "stop_reason"},
		{"documented max_tokens", stop(`"max_tokens"`), refusalIncomplete, ""},
		{"documented refusal", stop(`"refusal"`), refusalRefused, ""},
		{"documented interruption", stop(`"interrupted"`), refusalProviderFailed, ""},
		{"documented max_turns", terminal(`"max_turns"`), refusalProviderFailed + ": terminal_reason=max_turns", ""},
		{"stop_reason number", stop(`7`), refusalMalformed, ""},
		{"stop_reason oversized", stop(oversized), refusalMalformed, ""},
		{"terminal_reason null", terminal(`null`), refusalMalformed, ""},
		{"terminal_reason number", terminal(`1`), refusalMalformed, ""},
		{"terminal_reason oversized", terminal(oversized), refusalMalformed, ""},
		{"principal failure precedes unsupported terminal", mutateLine(mutateLine(terminal(canary), `"subtype":"success"`, `"subtype":"error_during_execution"`), `"is_error":false`, `"is_error":true`), refusalProviderFailed + ": error_during_execution", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), unsupportedCompletionLines(tc.line))
			requireUnsupportedVerdict(t, interpretation, answer, tc.want, tc.field)
			if frozen, _ := evaluateWriteTrace(t, newScopedInterpreter(WorkspaceWriteMode), unsupportedCompletionLines(tc.line)); tc.want == refusalUnsupportedCompletion &&
				(frozen.Verdict != task.VerdictRejected || frozen.Refusal != refusalMalformed) {
				t.Fatalf("frozen scoped revision changed its unknown-reason class: %+v", frozen)
			}
		})
	}
	for name, line := range map[string]string{
		"missing reasons": mutateLine(mutateLine(principal, `"stop_reason":"end_turn",`, ""), `"terminal_reason":"completed",`, ""),
		"null stop":       stop(`null`),
	} {
		interpretation, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), unsupportedCompletionLines(line))
		requireCommittedPrincipal(t, name, interpretation, answer)
	}
}

// requireUnsupportedVerdict requires the exact refusal and, for an
// unsupported completion, a sanitized diagnostic that names only the field.
func requireUnsupportedVerdict(t *testing.T, interpretation task.Interpretation, answer, want, field string) {
	t.Helper()
	if interpretation.Verdict != task.VerdictRejected || interpretation.Refusal != want || answer != "" {
		t.Fatalf("interpretation=%+v answer=%q want %q", interpretation, answer, want)
	}
	if err := task.ValidateInterpretation(interpretation); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(interpretation)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(unsupportedReasonCanary)) {
		t.Fatalf("provider reason leaked into the interpretation: %s", encoded)
	}
	if want != refusalUnsupportedCompletion {
		return
	}
	if len(interpretation.Diagnostics) == 0 || interpretation.Diagnostics[0].Code != "unsupported_completion" || interpretation.Diagnostics[0].Field != field {
		t.Fatalf("diagnostics=%+v want field %q", interpretation.Diagnostics, field)
	}
}

// TestCurrentUnsupportedFollowUpKeepsPrincipalPriority places unknown reasons
// after the principal result: they reject as unsupported unless the principal
// or an identity or syntax fault takes precedence.
func TestCurrentUnsupportedFollowUpKeepsPrincipalPriority(t *testing.T) {
	canary := `"` + unsupportedReasonCanary + `"`
	followUp := attributedResultLine("neutral follow-up", notificationAttribution(1), 1, "0.4982")
	unsupportedTerminal := withResultMetadata(traceFirstFollowUp, followUp, `"terminal_reason":"completed"`, `"terminal_reason":`+canary)
	unsupportedStop := withResultMetadata(traceFirstFollowUp, followUp, `"stop_reason":"end_turn"`, `"stop_reason":`+canary)
	principalStop := withResultMetadata(tracePrincipal, attributedResultLine(tracePrincipalText, `,"result_index":0`, 12, "0.4747"), `"stop_reason":"end_turn"`, `"stop_reason":`+canary)
	failedPrincipal := withTraceLine(tracePrincipal, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"`+claudeTestUUID+`","errors":["failed"],"result_index":0}`)
	both := func(first, second func([]string) []string) func([]string) []string {
		return func(lines []string) []string { return second(first(lines)) }
	}
	// A complete native negative principal settles the interactive transport,
	// so interactive replay reports the same canonical class as print; a
	// malformed stream and an unready session remain incomplete.
	for _, tc := range []struct {
		name        string
		mutate      func([]string) []string
		expected    task.SessionExpectation
		want, field string
		interactive string
	}{
		{"follow-up terminal", unsupportedTerminal, task.SessionExpectation{}, refusalUnsupportedCompletion, "terminal_reason", refusalUnsupportedCompletion},
		{"follow-up stop", unsupportedStop, task.SessionExpectation{}, refusalUnsupportedCompletion, "stop_reason", refusalUnsupportedCompletion},
		{"principal stop before follow-up terminal", both(principalStop, unsupportedTerminal), task.SessionExpectation{}, refusalUnsupportedCompletion, "stop_reason", refusalUnsupportedCompletion},
		{"failed principal before follow-up terminal", both(failedPrincipal, unsupportedTerminal), task.SessionExpectation{}, refusalProviderFailed + ": error_during_execution: failed", "", refusalProviderFailed + ": error_during_execution: failed"},
		{"identity before follow-up terminal", unsupportedTerminal, task.SessionExpectation{Required: true, ID: "123e4567-e89b-12d3-a456-426614174001"}, refusalIdentity, "", refusalIncomplete},
		{"syntax before follow-up terminal", func(lines []string) []string { return append(unsupportedTerminal(lines), "{") }, task.SessionExpectation{}, refusalMalformed, "", refusalIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := tc.expected
			if !expected.Required {
				expected = task.SessionExpectation{Required: true, ID: claudeTestUUID}
			}
			stdout := jsonl(tc.mutate(followUpTraceForMode(WorkspaceWriteMode))...)
			interpretation, answer := evaluateExpected(t, NewWorkspaceWriteInterpreter(), stdout, expected)
			requireUnsupportedVerdict(t, interpretation, answer, tc.want, tc.field)
			interactive := []byte(strings.Join(wrapInteractiveSession(tc.mutate(followUpTraceForMode(WorkspaceWriteMode))), "\n") + "\n")
			sealed, sealedAnswer := evaluateExpected(t, newInteractionInterpreter(WorkspaceWriteMode), interactive, expected)
			requireUnsupportedVerdict(t, sealed, sealedAnswer, tc.interactive, tc.field)
		})
	}
}

func evaluateExpected(t *testing.T, v predicate.Interpreter, stdout []byte, expected task.SessionExpectation) (task.Interpretation, string) {
	t.Helper()
	var answer bytes.Buffer
	input := predicate.Input{Seal: claudeTestSealForPredicate(stdout, v.Reference()), ExpectedSession: expected}
	interpretation, err := v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return interpretation, answer.String()
}

// TestCurrentRevisionsRecoverScopedSealsWithoutRewriting re-evaluates a
// sealed scoped trace that its own revision rejects only for opaque payload
// keys: the current revision publishes the principal answer, the original
// seal and evidence are unchanged, and the scoped revision stays replay-only.
func TestCurrentRevisionsRecoverScopedSealsWithoutRewriting(t *testing.T) {
	registry := retainedRegistry(t)
	opaque := `{"type":"assistant","session_id":"` + claudeTestUUID + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"x","input":{"a":1,"A":2}}]}}`
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := []string{interactiveInitLine(mode), opaque, principalLine()}
		requireScopedSealRecovers(t, registry, scopedReference(mode), ReferenceForMode(mode), lines)
		requireScopedSealRecovers(t, registry, scopedInteractionReference(mode), interactionReference(mode), wrapInteractiveSession(lines))
		if _, err := registry.ResolveRecovery(ReferenceForMode(mode)); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
			t.Fatalf("%s current revision became a recovery source: %v", mode, err)
		}
	}
}

// requireScopedSealRecovers requires source to reject lines on replay, its
// single recovery target to publish the principal answer without rewriting
// the seal or evidence, and source never to act as a recovery target.
func requireScopedSealRecovers(t *testing.T, registry predicate.Registry, source, target task.PredicateRef, lines []string) {
	t.Helper()
	stdout := []byte(strings.Join(lines, "\n") + "\n")
	original := bytes.Clone(stdout)
	input := revisionInput(stdout, source)
	replay, err := registry.Resolve(source)
	if err != nil {
		t.Fatal(err)
	}
	if rejected, replayErr := replay.Evaluate(input, &claudeTestEvidence{stdout: stdout}, new(bytes.Buffer)); replayErr != nil || rejected.Verdict != task.VerdictRejected {
		t.Fatalf("%s replay=%+v err=%v", source.Version, rejected, replayErr)
	}
	recovery, err := registry.ResolveRecovery(source)
	if err != nil || !recovery.Target().Equal(target) {
		t.Fatalf("%s recovery=%v err=%v", source.Version, recovery, err)
	}
	var answer bytes.Buffer
	recovered, err := recovery.Recover(input, &claudeTestEvidence{stdout: stdout}, &answer)
	if err != nil {
		t.Fatal(err)
	}
	requireCommittedPrincipal(t, source.Version, recovered, answer.String())
	if !input.Seal.Predicate.Equal(source) || !bytes.Equal(stdout, original) {
		t.Fatal("recovery rewrote the sealed source predicate or evidence")
	}
	frozen, ok := replay.(predicate.RecoveryInterpreter)
	if !ok {
		t.Fatalf("%s lost its recovery contract", source.Version)
	}
	if _, err = frozen.Recover(input, &claudeTestEvidence{stdout: stdout}, new(bytes.Buffer)); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("%s scoped revision became a recovery target: %v", source.Version, err)
	}
}

func TestExistingScopedProfilesRetainReferenceProbeAndPolicy(t *testing.T) {
	directory := fixtureClaudeEnvironment(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		request := profileRequest()
		request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
		requireFrozenReconstruction(t, request, ReferenceForMode(mode), scopedReference(mode), singleResultCompletion)
		interactive := request
		interactive.RequestedConfig.Interactive = true
		requireFrozenReconstruction(t, interactive, interactionReference(mode), scopedInteractionReference(mode), scopedAttributedCompletion)
		for _, reference := range []task.PredicateRef{scopedReference(mode), scopedInteractionReference(mode)} {
			if permission := nativePermissionForReference(mode, reference); permission != nativeDefaultPermission(mode) {
				t.Fatalf("%s %s permission=%q", mode, reference.Version, permission)
			}
			if flags := runtimeRequirementsForPredicate(request, reference).RequiredFlags; !reflect.DeepEqual(flags, RuntimeRequirements().RequiredFlags) {
				t.Fatalf("%s %s runtime flags=%v", mode, reference.Version, flags)
			}
		}
		crossed := request
		crossed.Mode, crossed.RequestedConfig.Permission = otherMode(mode), otherMode(mode)
		if _, err := normalizeNativePreparationOptions(crossed, nativePreparationOptions{predicateReference: scopedReference(mode)}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("%s request accepted the %s scoped revision: %v", crossed.Mode, mode, err)
		}
	}
}

// TestRecordIdentityObserverReadsTheAdvisoryProjection proves an opaque
// payload with repeated keys before init cannot hide the session identity,
// while an ambiguous session field still stops observation.
func TestRecordIdentityObserverReadsTheAdvisoryProjection(t *testing.T) {
	var got []task.SessionIdentity
	observer := newRecordIdentityObserver(t, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	hook := `{"type":"system","subtype":"hook_response","session_id":"` + claudeTestUUID + `","output":{"a":1,"A":2}}`
	for _, line := range []string{hook, minimalInitLine(WorkspaceWriteMode, `,"tools":[],"Tools":[]`)} {
		if err := observeClaudeRecord(observer, line); err != nil {
			t.Fatalf("observe %s: %v", line, err)
		}
	}
	ambiguous := mutateLine(writeInitLine(claudeTestUUID), `"cwd"`, `"Session_Id":"`+claudeTestUUID+`","cwd"`)
	if err := observeClaudeRecord(observer, ambiguous); err == nil {
		t.Fatal("ambiguous session field was observed")
	}
	if err := observer.Complete(); err != nil || len(got) != 1 || got[0] != (task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}) {
		t.Fatalf("identities=%+v err=%v", got, err)
	}
}

// TestObservedIdentityLeavesFrozenVerdictToItsPredicate pins the identity
// observer's boundary: its one advisory projection records the exact session
// once past an ambiguous opaque hook payload, and that recorded identity
// changes no verdict. Each sealed predicate still selects its own decoder, so
// the frozen scoped revisions reject the same stream and the current
// revisions publish the principal answer.
func TestObservedIdentityLeavesFrozenVerdictToItsPredicate(t *testing.T) {
	hook := `{"type":"system","subtype":"hook_response","session_id":"` + claudeTestUUID + `","output":{"a":1,"A":2}}`
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := []string{hook, interactiveInitLine(mode), principalLine()}
		recorded := requireObservedIdentityOnce(t, lines)
		for _, revision := range []struct {
			v      predicate.Interpreter
			trace  []string
			frozen bool
		}{
			{newScopedInterpreter(mode), lines, true},
			{newScopedInteractionInterpreter(mode), wrapInteractiveSession(lines), true},
			{NewInterpreter(mode), lines, false},
			{newInteractionInterpreter(mode), wrapInteractiveSession(lines), false},
		} {
			stdout := []byte(strings.Join(revision.trace, "\n") + "\n")
			input := revisionInput(stdout, revision.v.Reference())
			input.RecordedSession = &recorded
			var answer bytes.Buffer
			interpretation, err := revision.v.Evaluate(input, &claudeTestEvidence{stdout: stdout}, &answer)
			if err != nil {
				t.Fatal(err)
			}
			label := mode + " " + revision.v.Reference().Version
			if !revision.frozen {
				requireCommittedPrincipal(t, label, interpretation, answer.String())
			} else if interpretation.Verdict != task.VerdictRejected || answer.Len() != 0 {
				t.Fatalf("%s accepted the opaque hook after identity was observed: %+v", label, interpretation)
			}
		}
	}
}

// requireObservedIdentityOnce feeds lines to the one record identity
// observer and requires it to record the exact expected session once.
func requireObservedIdentityOnce(t *testing.T, lines []string) task.SessionIdentity {
	t.Helper()
	var got []task.SessionIdentity
	observer := newRecordIdentityObserver(t, func(identity task.SessionIdentity) error {
		got = append(got, identity)
		return nil
	})
	for _, line := range lines {
		if err := observeClaudeRecord(observer, line); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	want := task.SessionIdentity{Provider: Provider, ConversationID: claudeTestUUID}
	if err := observer.Complete(); err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("identities=%+v err=%v", got, err)
	}
	return got[0]
}
