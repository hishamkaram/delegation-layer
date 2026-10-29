package claude

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The retained unscoped revisions are the claude:print admissions made before
// message scope existed: the read-only native-permissions-v4 and workspace-write
// native-permissions-v5 references, and the interactive-v4 references. Their
// bytes, unscoped attribution policy, and end-of-stream behavior stay frozen;
// the frozen scoped digests of the same versions are pinned beside them.
const (
	unscopedReadDigest         = "f7c1053edce279071b9caa46f727cf196d5998fc05d07afdf9c5378acd9eb25c"
	scopedReadDigest           = "b3c26a3eae4e39d6a68f55e191b7ac2aa7f05476103ee2d111de0fd873880f0d"
	unscopedWriteDigest        = "333a689a864dfb03ecd4a6eace97bcf6964fea96768ddbbb86a92d98c4a7a76b"
	scopedWriteDigest          = "27bf2e76be4185559ecaa8cfbd404d81744281298b6a29b6bf99ab4e1f4b1b29"
	unscopedReadInteractive    = "4ae385f7773f6c9308186978b2727b584d18c367590d02e24997a6f699fb13ac"
	unscopedWriteInteractive   = "b5fe9380ce877f896a2ef0fa84a9a45365582aee182faa535a82e4574505f641"
	scopeOddParentTypeField    = `,"parent_tool_use_id":7`
	scopeOddParentAliasedField = `,"Parent_Tool_Use_Id":"toolu_scope_agent"`
)

func retainedRegistry(t *testing.T) predicate.Registry {
	t.Helper()
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// unscopedRevisions pairs each mode's retained unscoped digest with the frozen
// scoped digest that shares its version string.
var unscopedRevisions = []struct {
	mode, version, frozen, scoped string
}{
	{Mode, "native-permissions-v4", unscopedReadDigest, scopedReadDigest},
	{WorkspaceWriteMode, "native-permissions-v5", unscopedWriteDigest, scopedWriteDigest},
}

func otherMode(mode string) string {
	if mode == Mode {
		return WorkspaceWriteMode
	}
	return Mode
}

func TestUnscopedRevisionsKeepFrozenIdentity(t *testing.T) {
	for _, revision := range unscopedRevisions {
		alias, scoped := unscopedReference(revision.mode), scopedReference(revision.mode)
		if alias.Adapter != Provider || alias.Mode != revision.mode || alias.Version != revision.version || alias.SHA256 != revision.frozen ||
			task.ComputeSHA256([]byte(unscopedNativeContract(revision.mode))) != revision.frozen {
			t.Fatalf("%s retained unscoped reference changed: %+v", revision.mode, alias)
		}
		requireScopedRevisionIdentity(t, revision.mode, revision.version, revision.scoped, alias)
		if !isUnscopedReference(revision.mode, alias) || isUnscopedReference(revision.mode, scoped) || isUnscopedReference(otherMode(revision.mode), alias) {
			t.Fatalf("%s unscoped classification crossed a digest or mode", revision.mode)
		}
	}
}

// requireScopedRevisionIdentity requires the frozen scoped reference to keep
// the unscoped version string under its own pinned digest, and the current
// reference to use neither.
func requireScopedRevisionIdentity(t *testing.T, mode, version, digest string, alias task.PredicateRef) {
	t.Helper()
	scoped := scopedReference(mode)
	if scoped.Version != version || scoped.SHA256 != digest || scoped.SHA256 == alias.SHA256 ||
		task.ComputeSHA256([]byte(scopedNativeContract(mode))) != digest {
		t.Fatalf("%s scoped reference must keep the version string under its own digest: %+v", mode, scoped)
	}
	if current := ReferenceForMode(mode); current.Version == version || current.SHA256 == digest {
		t.Fatalf("%s current reference reused a frozen version or digest: %+v", mode, current)
	}
}

// TestUnscopedClassificationNeverMatchesTheZeroReference guards the zero
// reference, which equals the empty reference an unsupported mode yields.
func TestUnscopedClassificationNeverMatchesTheZeroReference(t *testing.T) {
	for _, mode := range []string{"", "read-write", "interactive"} {
		if !unscopedReference(mode).Equal(task.PredicateRef{}) || isUnscopedReference(mode, task.PredicateRef{}) {
			t.Fatalf("unsupported mode %q was given an unscoped revision", mode)
		}
	}
	if isUnscopedReference(Mode, task.PredicateRef{}) || isUnscopedReference(WorkspaceWriteMode, task.PredicateRef{}) {
		t.Fatal("the zero reference was classified as an unscoped revision")
	}
}

// TestUnscopedReadOnlyContractKeepsOriginalUnscopedText pins the frozen
// read-only bytes apart from the current ones: the original init clause and
// unscoped completion text, without the scoped read-only rewrite.
func TestUnscopedReadOnlyContractKeepsOriginalUnscopedText(t *testing.T) {
	frozen, current := unscopedNativeContract(Mode), scopedNativeContract(Mode)
	if len(frozen) != 4858 || task.ComputeSHA256([]byte(frozen)) != unscopedReadDigest || task.ComputeSHA256([]byte(current)) != scopedReadDigest {
		t.Fatalf("read-only contract bytes changed: frozen=%d bytes", len(frozen))
	}
	if !strings.Contains(frozen, "exactly one system/init object is required;") || !strings.Contains(frozen, unscopedCompletionAttributionContract) ||
		strings.Contains(frozen, scopedCompletionAttributionContract) || strings.Contains(frozen, "one initial system/init object") {
		t.Fatal("frozen read-only contract gained scoped text")
	}
	if strings.Contains(current, "exactly one system/init object is required;") || !strings.Contains(current, scopedCompletionAttributionContract) {
		t.Fatal("scoped read-only contract lost scoped text")
	}
}

func TestUnscopedInteractionRevisionsKeepFrozenIdentity(t *testing.T) {
	for mode, digest := range map[string]string{Mode: unscopedReadInteractive, WorkspaceWriteMode: unscopedWriteInteractive} {
		frozen, scoped := unscopedInteractionReference(mode), scopedInteractionReference(mode)
		if frozen.Version != "interactive-v4" || frozen.SHA256 != digest || scoped.Version != "interactive-v5" || scoped.SHA256 == digest {
			t.Fatalf("%s interactive references frozen=%+v scoped=%+v", mode, frozen, scoped)
		}
	}
}

func TestCurrentContractsStateMessageScopeAndInitialInit(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		current, frozen := ContractForMode(mode), unscopedNativeContract(mode)
		for _, want := range []string{"root assistant or user events after a result", "nested agent messages", "parent_tool_use_id", "malformed before or after the principal result", "leading acknowledgement", "empty string result", "num_turns explicitly 0"} {
			if !strings.Contains(current, want) {
				t.Fatalf("%s contract omits %q", mode, want)
			}
		}
		if strings.Contains(frozen, "parent_tool_use_id") || strings.Contains(frozen, "root assistant") || strings.Contains(frozen, "leading acknowledgement") {
			t.Fatal("frozen unscoped contract gained message-scope text")
		}
		if !strings.Contains(interactionContract(), "nested agent message") || !strings.Contains(interactionContract(), "leading acknowledgements") ||
			strings.Contains(unscopedInteractionContract, "parent_tool_use_id") || strings.Contains(unscopedInteractionContract, "leading acknowledgement") {
			t.Fatal("interactive revisions do not state their own scope policy inline")
		}
	}
	read := ContractForMode(Mode)
	if strings.Contains(read, "exactly one system/init") || !strings.Contains(read, "one initial system/init object is required, and any later system/init is governed only by completion_attribution") {
		t.Fatalf("read-only contract misstates initialization: %s", read)
	}
}

func TestRegistryResolvesRetainedAndCurrentRevisionsByExactDigest(t *testing.T) {
	registry := retainedRegistry(t)
	for _, revision := range unscopedRevisions {
		for _, reference := range []task.PredicateRef{
			unscopedReference(revision.mode), scopedReference(revision.mode), ReferenceForMode(revision.mode),
			unscopedInteractionReference(revision.mode), scopedInteractionReference(revision.mode), interactionReference(revision.mode),
		} {
			resolved, err := registry.Resolve(reference)
			if err != nil || !resolved.Reference().Equal(reference) {
				t.Fatalf("%s/%s resolved %v err=%v", reference.Mode, reference.Version, resolved, err)
			}
		}
		unknown := scopedReference(revision.mode)
		unknown.SHA256 = strings.Repeat("0", 64)
		if _, err := registry.Resolve(unknown); !errors.Is(err, task.ErrIncompatiblePredicate) {
			t.Fatalf("%s unknown digest under a retained version resolved: %v", revision.mode, err)
		}
	}
}

func TestRetainedRevisionsAreReplayOnlyAndNeverRecoverySources(t *testing.T) {
	registry := retainedRegistry(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		if _, err := registry.ResolveRecovery(unscopedReference(mode)); !errors.Is(err, predicate.ErrRecoveryUnsupported) {
			t.Fatalf("%s retained print alias gained a recovery role: %v", mode, err)
		}
		recovery, err := registry.ResolveRecovery(unscopedInteractionReference(mode))
		if err != nil || !recovery.Target().Equal(interactionReference(mode)) {
			t.Fatalf("%s interactive-v4 recovery=%v err=%v", mode, recovery, err)
		}
	}
	retained := []task.PredicateRef{unscopedReference(Mode), unscopedReference(WorkspaceWriteMode), unscopedInteractionReference(Mode), unscopedInteractionReference(WorkspaceWriteMode)}
	for _, interpreter := range Registration().Interpreters {
		if recovery, ok := interpreter.(predicate.RecoveryInterpreter); ok {
			requireRetainedRecoveryRoles(t, recovery, retained)
		}
	}
}

// requireRetainedRecoveryRoles requires only the current interactive revision
// to accept a retained source, and no retained revision to recover anything.
func requireRetainedRecoveryRoles(t *testing.T, recovery predicate.RecoveryInterpreter, retained []task.PredicateRef) {
	t.Helper()
	reference := recovery.Reference()
	for _, source := range retained {
		if recovery.CanRecover(source) != (reference.Equal(interactionReference(source.Mode)) && source.Version == "interactive-v4") {
			t.Fatalf("%s/%s recovery eligibility of %s/%s", reference.Mode, reference.Version, source.Mode, source.Version)
		}
	}
	if !isUnscopedReference(reference.Mode, reference) && reference.Version != "interactive-v4" {
		return
	}
	if _, err := recovery.Recover(predicate.Input{Seal: claudeTestSealForPredicate(nil, reference)}, &claudeTestEvidence{}, new(strings.Builder)); !errors.Is(err, task.ErrIncompatiblePredicate) {
		t.Fatalf("%s retained revision became a recovery target: %v", reference.Version, err)
	}
}

// followUpTraceForMode is the native follow-up trace with the init line of mode.
func followUpTraceForMode(mode string) []string {
	lines := nativeFollowUpTrace()
	for index, line := range lines {
		if line == writeInitLine(claudeTestUUID) {
			lines[index] = interactiveInitLine(mode)
		}
	}
	return lines
}

func TestUnscopedRevisionKeepsUnscopedInterpretation(t *testing.T) {
	registry := retainedRegistry(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		frozen, err := registry.Resolve(unscopedReference(mode))
		if err != nil {
			t.Fatal(err)
		}
		current := NewInterpreter(mode)
		nested := childScopeCases(mode, false)[0].lines
		if rejected, answer := evaluateWriteTrace(t, frozen, nested); rejected.Verdict != task.VerdictRejected || rejected.Refusal != refusalMalformed || answer != "" {
			t.Fatalf("%s unscoped revision accepted nested messages before a parent init: %+v answer=%q", mode, rejected, answer)
		}
		// The current revision frames every accepted root answer; nested
		// assistant text is never a section.
		if committed, answer := evaluateWriteTrace(t, current, nested); committed.Verdict != task.VerdictCommitted || answer != expectedResponses(t, nested) {
			t.Fatalf("%s current revision rejected nested messages: %+v answer=%q", mode, committed, answer)
		}
		// The unscoped revision never read the field: any value is an ordinary
		// root message, and the retained decoder label must not reject globally.
		initial, principal := interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 4, "0.1")
		followUp := attributedResultLine("neutral", notificationAttribution(1), 1, "0.2")
		for _, field := range []string{scopeOddParentTypeField, scopeOddParentAliasedField, `,"parent_tool_use_id":""`} {
			odd := []string{initial, principal, initial, scopeMessage("assistant", field, scopeTextBlock), followUp}
			if retained, answer := evaluateWriteTrace(t, frozen, odd); retained.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
				t.Fatalf("%s unscoped revision rejected %s: %+v answer=%q", mode, field, retained, answer)
			}
			if scoped, _ := evaluateWriteTrace(t, current, odd); scoped.Verdict != task.VerdictRejected {
				t.Fatalf("%s current revision accepted %s: %+v", mode, field, scoped)
			}
		}
	}
}

func TestUnscopedRevisionStillCommitsPayloadsWithoutNestedMessages(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		frozen, err := retainedRegistry(t).Resolve(unscopedReference(mode))
		if err != nil {
			t.Fatal(err)
		}
		for name, lines := range map[string][]string{
			"native follow-up trace": followUpTraceForMode(mode),
			"human indexed single":   {interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0,"origin":{"kind":"human"}`, 1, "0.1")},
			"legacy single":          {interactiveInitLine(mode), resultLine(claudeTestUUID, tracePrincipalText)},
		} {
			retained, retainedAnswer := evaluateWriteTrace(t, frozen, lines)
			scoped, scopedAnswer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
			// The retained revision stays principal-only; the current revision
			// frames every accepted root answer and keeps principal accounting.
			if retained.Verdict != task.VerdictCommitted || retainedAnswer != tracePrincipalText || scoped.Verdict != task.VerdictCommitted ||
				scopedAnswer != expectedResponses(t, lines) || !reflect.DeepEqual(retained.Usage, scoped.Usage) {
				t.Fatalf("%s %s retained=%+v/%q current=%+v/%q", mode, name, retained, retainedAnswer, scoped, scopedAnswer)
			}
		}
	}
}

func TestUnscopedInteractionRevisionKeepsUnscopedInterpretation(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		t.Run(mode+"/nested messages", func(t *testing.T) { requireInteractiveNestedMessageScope(t, mode) })
		t.Run(mode+"/ignored parent field", func(t *testing.T) { requireInteractiveIgnoredParentField(t, mode) })
		t.Run(mode+"/end of stream", func(t *testing.T) { requireInteractiveReopenedSettlement(t, mode) })
	}
}

func wrapInteractiveSession(lines []string) []string {
	return append(append([]string{interactiveRunning}, lines...), interactiveIdle)
}

// requireInteractiveNestedMessageScope requires interactive-v4 to reject a
// nested message before a parent init, as before message scope existed, while
// it keeps committing its native follow-up trace and interactive-v5 accepts
// the nested message.
func requireInteractiveNestedMessageScope(t *testing.T, mode string) {
	t.Helper()
	frozen, current := newUnscopedInteractionInterpreter(mode), newInteractionInterpreter(mode)
	nested := wrapInteractiveSession(childScopeCases(mode, false)[0].lines)
	if rejected, answer := evaluateInteractiveTrace(t, frozen, nested); rejected.Verdict != task.VerdictRejected || answer != "" {
		t.Fatalf("%s interactive-v4 accepted nested messages before a parent init: %+v answer=%q", mode, rejected, answer)
	}
	if committed, answer := evaluateInteractiveTrace(t, current, nested); committed.Verdict != task.VerdictCommitted || answer != expectedResponses(t, nested) {
		t.Fatalf("%s current interactive revision rejected nested messages: %+v answer=%q", mode, committed, answer)
	}
	if committed, answer := evaluateInteractiveTrace(t, frozen, interactiveFollowUpTrace(mode)); committed.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
		t.Fatalf("%s interactive-v4 no longer commits its native follow-up trace: %+v answer=%q", mode, committed, answer)
	}
}

// requireInteractiveIgnoredParentField requires interactive-v4 never to read
// parent_tool_use_id: an odd value is an ordinary root message.
func requireInteractiveIgnoredParentField(t *testing.T, mode string) {
	t.Helper()
	initial, principal := interactiveInitLine(mode), attributedResultLine(tracePrincipalText, `,"result_index":0`, 4, "0.1")
	odd := wrapInteractiveSession([]string{initial, principal, initial, scopeMessage("assistant", scopeOddParentTypeField, scopeTextBlock), attributedResultLine("neutral", notificationAttribution(1), 1, "0.2")})
	if retained, answer := evaluateInteractiveTrace(t, newUnscopedInteractionInterpreter(mode), odd); retained.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
		t.Fatalf("%s interactive-v4 rejected an ignored parent field: %+v answer=%q", mode, retained, answer)
	}
}

// requireInteractiveReopenedSettlement pins the end-of-stream columns of each
// attributed interactive revision. Interactive-v4 never re-checked the session
// at end of stream, so every reopened case commits the principal answer;
// interactive-v5 commits only the cases that are settled.
func requireInteractiveReopenedSettlement(t *testing.T, mode string) {
	t.Helper()
	frozen, current := newUnscopedInteractionInterpreter(mode), newInteractionInterpreter(mode)
	for _, tc := range reopenedSettlementCases(mode) {
		lines := append(closedInteractiveTrace(mode), tc.tail...)
		interpretation, answer := evaluateInteractiveTrace(t, frozen, lines)
		if interpretation.Verdict != task.VerdictCommitted || answer != tracePrincipalText {
			t.Fatalf("%s interactive-v4 end-of-stream %q: %+v answer=%q", mode, tc.name, interpretation, answer)
		}
		strict, strictAnswer := evaluateInteractiveTrace(t, current, lines)
		if (strict.Verdict == task.VerdictCommitted) != tc.settled || (tc.settled && strictAnswer != expectedResponses(t, lines)) {
			t.Fatalf("%s current interactive end-of-stream %q: %+v answer=%q", mode, tc.name, strict, strictAnswer)
		}
	}
}

// TestFrozenInteractiveV3SystemOnlyPostClosureInterpretationUnchanged pins the
// released single-result replay: it has no end-of-stream settlement check, so a
// closed invocation followed only by system events keeps its recorded verdict.
// The frozen attributed interactive-v4 keeps the same end-of-stream rule; only
// the scoped interactive-v5 requires the current idle settlement.
func TestFrozenInteractiveV3SystemOnlyPostClosureInterpretationUnchanged(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		frozen := newPreAttributionInteractionInterpreter(mode)
		tests := []struct {
			name       string
			tail       []string
			frozenWant string
			unscoped   string
			scoped     string
		}{
			{"session running at EOF", []string{interactiveRunning}, task.VerdictCommitted, task.VerdictCommitted, task.VerdictRejected},
			{"session requires action at EOF", []string{interactiveRequiresAction}, task.VerdictCommitted, task.VerdictCommitted, task.VerdictRejected},
			{"session idle again", []string{interactiveRunning, interactiveIdle}, task.VerdictCommitted, task.VerdictCommitted, task.VerdictCommitted},
			{"repeated init after closure", []string{interactiveInitLine(mode)}, task.VerdictRejected, task.VerdictRejected, task.VerdictRejected},
		}
		for _, tc := range tests {
			lines := append(closedInteractiveTrace(mode), tc.tail...)
			if got, answer := evaluateInteractiveTrace(t, frozen, lines); got.Verdict != tc.frozenWant || (got.Verdict == task.VerdictCommitted && answer != tracePrincipalText) {
				t.Fatalf("%s interactive-v3 %q changed: %+v answer=%q", mode, tc.name, got, answer)
			}
			attributed := []struct {
				interpreter predicate.Interpreter
				want        string
			}{{newUnscopedInteractionInterpreter(mode), tc.unscoped}, {newInteractionInterpreter(mode), tc.scoped}}
			for _, revision := range attributed {
				if got, _ := evaluateInteractiveTrace(t, revision.interpreter, lines); got.Verdict != revision.want {
					t.Fatalf("%s %s %q verdict=%s want %s", mode, revision.interpreter.Reference().Version, tc.name, got.Verdict, revision.want)
				}
			}
		}
	}
}

// TestFrozenLiveInteractionKeepsUnscopedToolResultResolution pins the retained
// live behavior that only the scoped revision changes: a nested agent's
// tool_result carrying a colliding tool_use_id resolves the root question.
func TestFrozenLiveInteractionKeepsUnscopedToolResultResolution(t *testing.T) {
	const question = `{"type":"control_request","request_id":"native-question","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"toolu_question","input":{"questions":[{"question":"Choose","options":[{"label":"Blue"}]}]}}}`
	request := task.TaskRecord{RootID: claudeTestRootID, TaskID: claudeTestTaskID, Mode: Mode}
	protocol, err := newClaudeInteractionWithCapture(request, task.SessionExpectation{Required: true, ID: claudeTestUUID}, nil, true, unscopedAttributedCompletion)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong protocol")
	}
	receiveClaudeRecord(t, p, interactiveInitLine(Mode))
	receiveClaudeRecord(t, p, question)
	nested := scopeMessage("user", scopeChildParent, `{"type":"tool_result","tool_use_id":"toolu_question","content":"answered"}`)
	if update := receiveClaudeRecord(t, p, nested); len(update.Events) != 1 || update.Events[0].Kind != "resolved" || len(p.pending) != 0 {
		t.Fatalf("retained unscoped resolution changed: %+v pending=%d", update.Events, len(p.pending))
	}
}

func TestExistingUnscopedProfilesRetainReferenceProbeAndPolicy(t *testing.T) {
	directory := fixtureClaudeEnvironment(t)
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		request := profileRequest()
		request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
		alias := unscopedReference(mode)
		requireFrozenReconstruction(t, request, ReferenceForMode(mode), alias, singleResultCompletion)
		if permission := nativePermissionForReference(mode, alias); permission != nativeDefaultPermission(mode) {
			t.Fatalf("%s retained alias permission=%q", mode, permission)
		}
		if flags := runtimeRequirementsForPredicate(request, alias).RequiredFlags; !reflect.DeepEqual(flags, RuntimeRequirements().RequiredFlags) {
			t.Fatalf("%s retained alias runtime flags=%v", mode, flags)
		}
		// Each alias is bound to its own mode: no request of the other mode may
		// reconstruct from it.
		crossed := request
		crossed.Mode, crossed.RequestedConfig.Permission = otherMode(mode), otherMode(mode)
		if _, err := normalizeNativePreparationOptions(crossed, nativePreparationOptions{predicateReference: alias}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("%s request accepted the %s alias: %v", crossed.Mode, mode, err)
		}
		interactive := request
		interactive.RequestedConfig.Interactive = true
		requireFrozenReconstruction(t, interactive, interactionReference(mode), unscopedInteractionReference(mode), unscopedAttributedCompletion)
	}
}
