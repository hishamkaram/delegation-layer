package claude

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// eofExpectation is the end-of-stream outcome of one interactive revision for
// one stream: the sealed verdict and whether live Complete() faults.
type eofExpectation struct {
	sealed    string
	liveFault bool
}

// eofRevisionCase is one stream that follows a validly closed invocation, with
// the expectation of each attributed interactive revision. The frozen column is
// the executed verdict of interactive-v4 at its original admission; the scoped
// column is the strict current interactive-v5 rule.
type eofRevisionCase struct {
	name   string
	tail   []string
	frozen eofExpectation
	scoped eofExpectation
}

// eofRevisionCases covers evidence after input closure. Interactive-v4 checked
// only its sticky closure and reducer at end of stream, so a settled cycle
// with the session still running commits; a repeated init without its result
// is accepted live but rejected by the sealed reducer. Interactive-v5 also
// requires the current idle, agent, question, cycle, and batch settlement.
func eofRevisionCases(mode string) []eofRevisionCase {
	follow := reopenedFollowUp(mode)
	with := func(prefix []string, suffix ...string) []string {
		return append(append(append([]string{}, prefix...), follow...), suffix...)
	}
	committed := eofExpectation{sealed: task.VerdictCommitted}
	rejected := eofExpectation{sealed: task.VerdictRejected}
	faulted := eofExpectation{sealed: task.VerdictRejected, liveFault: true}
	return []eofRevisionCase{
		{"completed cycle, session running at EOF", with([]string{interactiveRunning}), committed, faulted},
		{"completed cycle, then idle", with([]string{interactiveRunning}, interactiveIdle), committed, committed},
		{"completed cycle, action required at EOF", with([]string{interactiveRunning}, interactiveRequiresAction), committed, faulted},
		{"agent settled before cycle, then idle", with([]string{interactiveRunning, interactiveAgent, interactiveSettled}, interactiveIdle), committed, committed},
		{"agent unsettled after cycle, then idle", with([]string{interactiveRunning, interactiveAgent}, interactiveIdle), committed, faulted},
		{"session running at EOF", []string{interactiveRunning}, committed, faulted},
		{"repeated init without its result", []string{interactiveInitLine(mode)}, rejected, faulted},
		{"cycle opened and never answered", []string{interactiveRunning, interactiveInitLine(mode), interactiveAssistant("neutral"), interactiveIdle}, rejected, faulted},
	}
}

// interactiveProfileFor builds the interactive profile that launches a
// task admitted under reference: fresh preparation for the current revision,
// reconstruction from the recorded predicate for a retained one.
func interactiveProfileFor(t *testing.T, mode string, reference task.PredicateRef) commonprovider.PreparedProfile {
	t.Helper()
	request := profileRequest()
	request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = fixtureClaudeEnvironment(t), mode, mode
	request.RequestedConfig.Interactive = true
	fresh, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: fresh.Inspection.Executable, SHA256: fresh.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fresh.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	profile := current
	if !current.Plan.Predicate.Equal(reference) {
		existing, existingErr := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, EffectiveConfig: current.Effective, Environment: current.Plan.Environment, EnvironmentRecorded: true})
		if existingErr != nil {
			t.Fatal(existingErr)
		}
		if profile, err = existing.Finalize(facts, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !profile.Plan.Predicate.Equal(reference) || profile.Interaction == nil {
		t.Fatalf("%s profile predicate=%+v interaction=%v", mode, profile.Plan.Predicate, profile.Interaction != nil)
	}
	return profile
}

// completeAfter feeds lines to a live protocol built by the profile and
// returns Complete(). The closed prefix must complete before the tail.
func completeAfter(t *testing.T, profile commonprovider.PreparedProfile, mode string, tail []string) error {
	t.Helper()
	protocol, err := profile.Interaction(task.SessionExpectation{Required: true, ID: claudeTestUUID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live, ok := protocol.(*claudeInteraction)
	if !ok {
		t.Fatal("wrong live protocol")
	}
	for _, line := range closedInteractiveTrace(mode) {
		receiveClaudeRecord(t, live, line)
	}
	if err = live.Complete(); err != nil {
		t.Fatalf("%s closed trace did not complete: %v", profile.Plan.Predicate.Version, err)
	}
	for _, line := range tail {
		receiveClaudeRecord(t, live, line)
	}
	return live.Complete()
}

// eofRevision is one interactive revision under test, reached the way the
// runtime reaches it: sealed replay through exact registry resolution and live
// evaluation through the reconstructed PreparedProfile.Interaction.
type eofRevision struct {
	label   string
	mode    string
	sealed  predicate.Interpreter
	profile commonprovider.PreparedProfile
}

func newEOFRevision(t *testing.T, label, mode string, reference task.PredicateRef) eofRevision {
	t.Helper()
	sealed, err := retainedRegistry(t).Resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	return eofRevision{label: label, mode: mode, sealed: sealed, profile: interactiveProfileFor(t, mode, reference)}
}

// require checks the revision's sealed verdict, published answer, and live
// Complete() against want for the stream that follows the closed invocation.
func (r eofRevision) require(t *testing.T, tail []string, want eofExpectation) {
	t.Helper()
	got, answer := evaluateInteractiveTrace(t, r.sealed, append(closedInteractiveTrace(r.mode), tail...))
	if got.Verdict != want.sealed || (got.Verdict == task.VerdictCommitted) != (answer == tracePrincipalText) {
		t.Fatalf("%s sealed=%+v answer=%q want %s", r.label, got, answer, want.sealed)
	}
	err := completeAfter(t, r.profile, r.mode, tail)
	if want.liveFault && !errors.Is(err, task.ErrEvidenceFault) || !want.liveFault && err != nil {
		t.Fatalf("%s live Complete=%v want fault=%v", r.label, err, want.liveFault)
	}
}

// TestInteractiveRevisionsDifferOnlyInEndOfStreamSettlement pins the frozen
// interactive-v4 end-of-stream rule beside the strict interactive-v5 one, in
// both permission modes.
func TestInteractiveRevisionsDifferOnlyInEndOfStreamSettlement(t *testing.T) {
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		frozen := newEOFRevision(t, "interactive-v4", mode, unscopedInteractionReference(mode))
		scoped := newEOFRevision(t, "interactive-v5", mode, interactionReference(mode))
		for _, tc := range eofRevisionCases(mode) {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				frozen.require(t, tc.tail, tc.frozen)
				scoped.require(t, tc.tail, tc.scoped)
			})
		}
	}
}
