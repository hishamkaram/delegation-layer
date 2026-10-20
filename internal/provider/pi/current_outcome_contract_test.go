package pi

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// priorCurrentWire lists, per mode, every earlier revision that reads the
// current wire and keeps its pre-v9 evidence policy frozen.
func priorCurrentWire(mode string) []predicate.Interpreter {
	if mode == ModeReadOnly {
		return []predicate.Interpreter{newPriorCurrentInterpreter(mode), newHistoricalV5Interpreter(mode)}
	}
	return []predicate.Interpreter{newPriorCurrentInterpreter(mode), newHistoricalV7Interpreter(mode), newHistoricalV5Interpreter(mode)}
}

func evaluateWith(t *testing.T, interpreter predicate.Interpreter, stdout []byte) piOutcomeResult {
	t.Helper()
	var answer bytes.Buffer
	interpretation, err := interpreter.Evaluate(
		predicate.Input{Seal: validSeal(interpreter.Reference(), stdout, nil)}, evidence{stdout: stdout}, &answer,
	)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	return piOutcomeResult{interpretation: interpretation, answer: answer.String()}
}

func TestPiPriorCurrentRevisionsKeepTheirFrozenEvidencePolicy(t *testing.T) {
	base := piStream(testUUID, "frozen answer", true)
	retried := piNativeRetryStream(testUUID, "frozen retry", true)
	retryFailed := []byte(strings.Replace(string(retried), `"auto_retry_end","attempt":1,"success":true`, `"auto_retry_end","attempt":1,"success":false`, 1))
	for _, mode := range piOutcomeModes {
		for _, interpreter := range priorCurrentWire(mode) {
			ref := interpreter.Reference()
			t.Run(mode+"/v"+ref.Version, func(t *testing.T) {
				// Frozen bytes and interpretation: explicit native failure fields were
				// not selected, so otherwise complete evidence still commits.
				requirePiOutcomeCommitted(t, evaluateWith(t, interpreter, withPiLines(base, `{"type":"agent_settled","aborted":true}`)), "frozen answer")
				requirePiOutcomeCommitted(t, evaluateWith(t, interpreter, withPiLines(base, `{"type":"agent_settled","aborted":"x","aborted":null}`)), "frozen answer")
				requirePiOutcomeCommitted(t, evaluateWith(t, interpreter, retryFailed), "frozen retry")
				// The display event keeps the generic unknown-lifecycle rejection
				// before and after settlement.
				requirePiOutcomeRejected(t, evaluateWith(t, interpreter, withPiLines(base, `{"type":"session_info_changed","name":"x"}`)), refusalMalformed)
				requirePiOutcomeRejected(t, evaluateWith(t, interpreter, withPiLines(base, `{"type":"agent_settled"}`, `{"type":"session_info_changed"}`)), refusalMalformed)
			})
		}
	}
}

func TestPiPriorCurrentRevisionsAreReplayOnly(t *testing.T) {
	for _, mode := range piOutcomeModes {
		current := NewInterpreter(mode).Reference()
		for _, interpreter := range priorCurrentWire(mode) {
			recoverable, ok := interpreter.(predicate.RecoveryInterpreter)
			if !ok {
				t.Fatalf("v%s interpreter does not expose recovery", interpreter.Reference().Version)
			}
			if recoverable.CanRecover(current) || recoverable.CanRecover(interpreter.Reference()) {
				t.Fatalf("v%s interpreter became a recovery target", interpreter.Reference().Version)
			}
			_, err := recoverable.Recover(predicate.Input{Seal: validSeal(interpreter.Reference(), nil, nil)}, evidence{}, &bytes.Buffer{})
			if !errors.Is(err, task.ErrIncompatiblePredicate) {
				t.Fatalf("v%s Recover error=%v", interpreter.Reference().Version, err)
			}
		}
	}
}

func TestPiRecoveryRegistryResolvesOneCurrentTargetPerMode(t *testing.T) {
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range piOutcomeModes {
		target := ReferenceForMode(mode)
		for _, source := range recoverableReferencesForMode(mode) {
			recovery, resolveErr := registry.ResolveRecovery(source)
			if resolveErr != nil || !recovery.Target().Equal(target) {
				t.Fatalf("%s v%s recovery=%v err=%v", mode, source.Version, recovery, resolveErr)
			}
		}
		if _, resolveErr := registry.ResolveRecovery(target); !errors.Is(resolveErr, predicate.ErrRecoveryUnsupported) {
			t.Fatalf("%s current predicate resolved a recovery target: %v", mode, resolveErr)
		}
	}
	for _, unrecoverable := range []task.PredicateRef{readOnlyV4Reference(), workspaceWriteV5Reference(), workspaceWriteV4Reference(), readOnlyV3Reference(), LegacyReference()} {
		if _, resolveErr := registry.ResolveRecovery(unrecoverable); !errors.Is(resolveErr, predicate.ErrRecoveryUnsupported) {
			t.Fatalf("v%s mode %s unexpectedly recoverable: %v", unrecoverable.Version, unrecoverable.Mode, resolveErr)
		}
	}
	if _, resolveErr := registry.ResolveRecovery(workspaceWriteV8Reference()); resolveErr != nil {
		t.Fatalf("workspace-write v8 recovery: %v", resolveErr)
	}
	crossMode, err := registry.ResolveRecovery(readOnlyV6Reference())
	if err != nil || crossMode.Target().Mode != ModeReadOnly {
		t.Fatalf("read-only v6 resolved across modes: %v %v", crossMode, err)
	}
}

func TestPiRecoveryReassessesPriorSealedEvidenceWithoutChangingTheSeal(t *testing.T) {
	complete := piStream(testUUID, "sealed answer", true)
	abortedEvidence := withPiLines(complete, `{"type":"agent_settled","aborted":true}`)
	for _, mode := range piOutcomeModes {
		current := NewInterpreter(mode)
		recovery, ok := current.(predicate.RecoveryInterpreter)
		if !ok {
			t.Fatal("current interpreter does not expose recovery")
		}
		for _, source := range recoverableReferencesForMode(mode) {
			t.Run(mode+"/v"+source.Version, func(t *testing.T) {
				recoverFrom := func(stdout []byte) (task.Interpretation, string, task.ProviderExitRecord) {
					seal := validSeal(source, stdout, nil)
					var answer bytes.Buffer
					interpretation, err := recovery.Recover(predicate.Input{Seal: seal}, evidence{stdout: stdout}, &answer)
					if err != nil {
						t.Fatal(err)
					}
					return interpretation, answer.String(), seal
				}
				// A sealed explicit abort must never recover into success.
				interpretation, answer, seal := recoverFrom(abortedEvidence)
				requirePiOutcomeRejected(t, piOutcomeResult{interpretation: interpretation, answer: answer}, refusalProviderFailed)
				if !seal.Predicate.Equal(source) {
					t.Fatalf("recovery mutated the sealed predicate: %+v", seal.Predicate)
				}
				// Compatible sealed wire that satisfies the new policy still recovers.
				interpretation, answer, _ = recoverFrom(withPiLines(complete, `{"type":"session_info_changed","name":"x"}`))
				requirePiOutcomeCommitted(t, piOutcomeResult{interpretation: interpretation, answer: answer}, "sealed answer")
			})
		}
	}
}

func TestPiV9ContractBindsOutcomePolicyAndPriorBytesStayDistinct(t *testing.T) {
	for _, mode := range piOutcomeModes {
		contract := Contract(mode)
		for _, phrase := range []string{
			`"version":"9"`,
			"agent_settled.aborted=true",
			"auto_retry_end.success=false",
			"session_info_changed and thinking_level_changed",
		} {
			if !strings.Contains(contract, phrase) {
				t.Fatalf("%s v9 contract is missing %q", mode, phrase)
			}
		}
		prior := contractForVersion(mode, priorVersionForMode(mode))
		if prior == "" || prior == contract || strings.Contains(prior, "agent_settled.aborted") {
			t.Fatalf("%s prior contract changed or aliases v9", mode)
		}
		if ReferenceForMode(mode).SHA256 != ContractDigest(mode) || ReferenceForMode(mode).Equal(priorReferenceForMode(mode)) {
			t.Fatalf("%s v9 reference is not distinct and bound to its contract", mode)
		}
	}
	if got, want := Version, "9"; got != want {
		t.Fatalf("exported adapter version=%q want %q", got, want)
	}
}

func TestPiReconstructionKeepsPriorFreshDefaultRecordsOnTheirRecordedPredicate(t *testing.T) {
	for _, reference := range []task.PredicateRef{readOnlyV6Reference(), workspaceWriteV8Reference()} {
		t.Run(reference.Mode, func(t *testing.T) {
			request, now := profileReconstructionFixture(t, reference.Mode)
			fresh := finalizeProfile(t, mustPrepareCandidate(t, request), now)
			if !fresh.Plan.Predicate.Equal(ReferenceForMode(reference.Mode)) || fresh.Plan.Predicate.Equal(reference) {
				t.Fatalf("fresh admission predicate=%+v", fresh.Plan.Predicate)
			}
			candidate, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, EffectiveConfig: fresh.Effective})
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := finalizeProfile(t, candidate, now)
			// The recorded predicate is retained while the launch contract of the
			// prior fresh default (current tools, startup controls, runtime
			// requirements) is reconstructed exactly.
			if !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(rebuilt.Plan.Arguments, fresh.Plan.Arguments) || !task.CompareEffectiveConfigs(rebuilt.Effective, fresh.Effective) {
				t.Fatalf("prior record reconstruction changed: %+v", rebuilt.Plan)
			}
			wantFlags := RuntimeRequirements(reference.Mode).RequiredFlags
			if candidate.Inspection == nil || candidate.Inspection.Runtime == nil || !slices.Equal(candidate.Inspection.Runtime.RequiredFlags, wantFlags) {
				t.Fatalf("prior record runtime requirements=%+v want %q", candidate.Inspection, wantFlags)
			}
		})
	}
}

type piSuccessorFixture struct {
	t       *testing.T
	catalog commonprovider.Catalog
	request task.TaskRecord
	fresh   commonprovider.PreparedProfile
	now     time.Time
}

func newPiSuccessorFixture(t *testing.T, mode string) piSuccessorFixture {
	t.Helper()
	request, now := profileReconstructionFixture(t, mode)
	fresh := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testUUID, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	return piSuccessorFixture{t: t, catalog: catalog, request: request, fresh: fresh, now: now}
}

func (f piSuccessorFixture) meta(predecessor task.PredicateRef) task.MetaRecord {
	return task.MetaRecord{Predicate: predecessor, EffectiveConfig: f.fresh.Effective}
}

func (f piSuccessorFixture) successor(predecessor task.PredicateRef) commonprovider.PreparedProfile {
	f.t.Helper()
	candidate, err := f.catalog.SuccessorCandidate(f.request, f.meta(predecessor))
	if err != nil {
		f.t.Fatal(err)
	}
	return finalizeProfile(f.t, candidate, f.now)
}

func (f piSuccessorFixture) recorded(predecessor task.PredicateRef) commonprovider.PreparedProfile {
	f.t.Helper()
	candidate, err := f.catalog.ExistingCandidate(f.request, f.meta(predecessor))
	if err != nil {
		f.t.Fatal(err)
	}
	return finalizeProfile(f.t, candidate, f.now)
}

func TestPiSuccessorUsesCurrentPolicyOnlyForPriorFreshDefaultPredecessors(t *testing.T) {
	for _, mode := range piOutcomeModes {
		t.Run(mode, func(t *testing.T) {
			fixture := newPiSuccessorFixture(t, mode)
			current, prior := ReferenceForMode(mode), priorReferenceForMode(mode)
			upgraded := fixture.successor(prior)
			if !upgraded.Plan.Predicate.Equal(current) || !slices.Contains(upgraded.Plan.Arguments, "--session") {
				t.Fatalf("successor of prior default: %+v", upgraded.Plan)
			}
			// Only the predicate changes: the recorded launch contract is retained.
			recorded := fixture.recorded(prior)
			if !slices.Equal(recorded.Plan.Arguments, upgraded.Plan.Arguments) || !recorded.Plan.Predicate.Equal(prior) {
				t.Fatalf("recorded reconstruction changed: %+v", recorded.Plan)
			}
			if kept := fixture.successor(current); !kept.Plan.Predicate.Equal(current) {
				t.Fatalf("successor of a current predecessor: %+v", kept.Plan.Predicate)
			}
			for _, older := range recoverableReferencesForMode(mode)[1:] {
				if kept := fixture.successor(older); !kept.Plan.Predicate.Equal(older) {
					t.Fatalf("successor of v%s was upgraded to %+v", older.Version, kept.Plan.Predicate)
				}
			}
		})
	}
}

func TestPiCurrentPolicyEvaluatesThroughCatalogRegistry(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	registry := catalog.Registry()
	stdout := withPiLines(piStream(testUUID, "catalog answer", true), `{"type":"agent_settled","aborted":true}`)
	for _, mode := range piOutcomeModes {
		for reference, wantVerdict := range map[string]string{ReferenceForMode(mode).Version: task.VerdictRejected, priorVersionForMode(mode): task.VerdictCommitted} {
			resolved, resolveErr := registry.Resolve(referenceForVersion(mode, reference))
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if got := evaluateWith(t, resolved, stdout).interpretation.Verdict; got != wantVerdict {
				t.Fatalf("%s v%s verdict=%q want %q", mode, reference, got, wantVerdict)
			}
		}
	}
}
