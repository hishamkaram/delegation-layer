package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// TestContinuationLeavesPredecessorOutputPredicateFrozen proves the other half
// of the upgrade contract: the sealed predecessor keeps rebuilding under its
// exact source predicate, and that source keeps rejecting follow-up evidence.
func TestContinuationLeavesPredecessorOutputPredicateFrozen(t *testing.T) {
	registry := NativeCatalog().Registry()
	for _, testCase := range outputUpgradeCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			interpreter, err := registry.Resolve(testCase.source)
			resolved := err == nil && interpreter != nil
			if matches := resolved && interpreter.Reference().Equal(testCase.source); !matches {
				t.Fatalf("source %+v resolved an interpreter=%t, its reference matches the source=%t, err=%v", testCase.source, resolved, matches, err)
			}
			fixture := newOutputUpgradeFixture(t, testCase)
			request, meta := readOutputUpgradeTask(t, fixture, outputUpgradePredecessorID)
			profile, err := prepareMatchedProfile(fixture.deps, fixture.root, request, meta)
			if err != nil || !profile.Plan.Predicate.Equal(testCase.source) || !meta.Predicate.Equal(testCase.source) {
				t.Fatalf("predecessor rebuilt under %+v (stored %+v), err=%v, want its source %+v", profile.Plan.Predicate, meta.Predicate, err, testCase.source)
			}
			// The shared recheck fixture seals native follow-up evidence under the
			// source and requires the original rejection to be published. A frozen
			// scoped source accepts the neutral cycle, so its fixture adds the
			// unknown content block only its advisory target accepts.
			sealedProviderRecheckTask(t, registry, providerRecheckSource{
				provider: testCase.provider, opaqueContent: isScopedRecheckSource(testCase.source),
				reference: testCase.source, target: testCase.target,
			})
		})
	}
}

// TestOpenCodeHistoricalRunTargetPublishesWhatItsSourcesReject is the
// historical group's positive control. Each task is admitted from a coherent
// run record and seals the same repeated-step-start evidence: the run-v5 target
// publishes the principal answer, and every eligible v2-v4 source rejects it.
func TestOpenCodeHistoricalRunTargetPublishesWhatItsSourcesReject(t *testing.T) {
	cases := outputUpgradeCases(t)
	for _, control := range outputUpgradeControlCases(t) {
		if control.provider == openCodeProvider {
			t.Run(control.mode, func(t *testing.T) {
				requireOpenCodeHistoricalRunControl(t, control, cases)
			})
		}
	}
}

// requireOpenCodeHistoricalRunControl seals the control's target evidence
// under the run-v5 target and under each eligible source of the same mode.
func requireOpenCodeHistoricalRunControl(t *testing.T, control outputUpgradeCase, cases []outputUpgradeCase) {
	t.Helper()
	fixture := newOutputUpgradeFixture(t, control)
	stream := control.successorStream(fixture.session)
	outcome, answer := sealOpenCodeHistoricalRun(t, fixture, strings.Repeat("b", 32), control.target, stream)
	if outcome.Verdict != task.VerdictCommitted || answer != outputUpgradeAnswer {
		t.Fatalf("run target %+v: verdict=%q answer=%q diagnostics=%+v, want the committed principal answer", control.target, outcome.Verdict, answer, outcome.Diagnostics)
	}
	sources := 0
	for index, testCase := range cases {
		if testCase.provider != openCodeProvider || testCase.mode != control.mode {
			continue
		}
		sources++
		if outcome, _ = sealOpenCodeHistoricalRun(t, fixture, fmt.Sprintf("%032x", index+1), testCase.source, stream); outcome.Verdict != task.VerdictRejected {
			t.Errorf("run source %+v published the evidence only its target accepts: verdict=%q", testCase.source, outcome.Verdict)
		}
	}
	if sources != 3 {
		t.Fatalf("%d eligible run sources for %s, want v2, v3 and v4", sources, control.mode)
	}
}

// sealOpenCodeHistoricalRun admits taskID in the fixture's store from a
// historical run record under reference, seals stdout and returns the
// collected outcome with any published answer.
func sealOpenCodeHistoricalRun(t *testing.T, f *outputUpgradeFixture, taskID string, reference task.PredicateRef, stdout string) (*task.OutcomeRecord, string) {
	t.Helper()
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeStore(t, store)
	brief := []byte("historical brief")
	request, err := admission.BuildRequest(store.RootID, taskID, openCodeProvider, f.workspace, f.testCase.requestedConfig(), brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := tasklifecycle.NewMeta(request, openCodeHistoricalRunProfile(t, f.deps.Catalog, request, f.testCase.revision, reference), f.supervisor.Binding(), f.runner, task.RunnerOwnershipManaged, outputUpgradeBuild)
	td, err := store.CreateTask(taskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOpenCodeTask(t, td)
	outcome := sealOutputUpgradeTask(t, td, openCodeProvider, f.session, reference, stdout)
	if outcome.Verdict != task.VerdictCommitted {
		return outcome, ""
	}
	answer, err := os.ReadFile(filepath.Join(td.Dir, outcome.Payload.Basename))
	if err != nil {
		t.Fatal(err)
	}
	return outcome, string(answer)
}

// outputUpgradePreparation is the part of an admitted preparation that must
// not change when only the output predicate is upgraded.
type outputUpgradePreparation struct {
	plan             execution.Plan
	effective        []byte
	writableRoots    []string
	preflight        string
	inspectionDigest string
	predicate        task.PredicateRef
}

// differingPreparationComponents names the components that differ. Values stay
// out of failure output: the plan and effective configuration carry the launch
// environment and host paths.
func differingPreparationComponents(a, b outputUpgradePreparation) []string {
	var names []string
	for _, component := range []struct {
		name string
		same bool
	}{
		{"plan", reflect.DeepEqual(a.plan, b.plan)},
		{"effective configuration", bytes.Equal(a.effective, b.effective)},
		{"writable roots", slices.Equal(a.writableRoots, b.writableRoots)},
		{"runtime preflight", a.preflight == b.preflight},
		{"inspection digest", a.inspectionDigest == b.inspectionDigest},
		{"predicate", a.predicate.Equal(b.predicate)},
	} {
		if !component.same {
			names = append(names, component.name)
		}
	}
	return names
}

// TestEligibleSourcePreparationDiffersOnlyByPredicate compares the production
// preparation of one successor request under each exact source and its target.
// The launch arguments, environment, roots, effective configuration, runtime
// capability, and inspection must be identical; only the predicate differs.
func TestEligibleSourcePreparationDiffersOnlyByPredicate(t *testing.T) {
	for _, testCase := range outputUpgradeCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			installOutputUpgradeProvider(t, testCase.executable())
			request := outputUpgradeSuccessorRequest(t, testCase)
			source := outputUpgradeProductionPreparation(t, testCase, request, testCase.source, commonprovider.Catalog.ExistingCandidate)
			target := outputUpgradeProductionPreparation(t, testCase, request, testCase.target, commonprovider.Catalog.ExistingCandidate)
			if !source.predicate.Equal(testCase.source) || !target.predicate.Equal(testCase.target) {
				t.Fatalf("predicates source=%+v target=%+v", source.predicate, target.predicate)
			}
			source.predicate, target.predicate = task.PredicateRef{}, task.PredicateRef{}
			if !reflect.DeepEqual(source, target) {
				t.Errorf("preparation differs beyond the predicate in: %s", strings.Join(differingPreparationComponents(source, target), ", "))
			}
		})
	}
}

// TestSuccessorOfEligibleSourcePreparesTheCurrentTarget compares the new-task
// successor operation on a predecessor stored under each exact source with the
// exact preparation of a task stored under the target. The successor must be
// the same preparation, predicate included; the immutable reconstruction of the
// source keeps its own predicate. An OpenCode successor changes launch dialect;
// opencode_acp_continuation_test.go owns its preparation contract.
func TestSuccessorOfEligibleSourcePreparesTheCurrentTarget(t *testing.T) {
	for _, testCase := range sameWireOutputUpgradeCases(outputUpgradeCases(t)) {
		t.Run(testCase.name, func(t *testing.T) {
			installOutputUpgradeProvider(t, testCase.executable())
			request := outputUpgradeSuccessorRequest(t, testCase)
			successor := outputUpgradeProductionPreparation(t, testCase, request, testCase.source, commonprovider.Catalog.SuccessorCandidate)
			want := outputUpgradeProductionPreparation(t, testCase, request, testCase.target, commonprovider.Catalog.ExistingCandidate)
			if !successor.predicate.Equal(testCase.target) {
				t.Fatalf("successor predicate %+v, want %+v", successor.predicate, testCase.target)
			}
			if !reflect.DeepEqual(successor, want) {
				t.Errorf("successor preparation differs from the target preparation in: %s", strings.Join(differingPreparationComponents(successor, want), ", "))
			}
		})
	}
}

func outputUpgradeSuccessorRequest(t *testing.T, testCase outputUpgradeCase) task.TaskRecord {
	t.Helper()
	rootID := strings.Repeat("a", 32)
	session, err := testCase.freshSession(rootID, outputUpgradePredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	prior := &task.PriorSession{Provider: testCase.provider, ConversationID: session, PredecessorTaskID: outputUpgradePredecessorID}
	request, err := admission.BuildRequest(rootID, strings.Repeat("c", 32), testCase.provider, testsupport.CanonicalTempDir(t), testCase.requestedConfig(), []byte("follow-up"), prior)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// outputUpgradeCandidateSource is the catalog operation that turns a stored
// predecessor record into the candidate under test.
type outputUpgradeCandidateSource func(commonprovider.Catalog, task.TaskRecord, task.MetaRecord) (commonprovider.ProfileCandidate, error)

// outputUpgradeProductionPreparation prepares request from a record stored
// under reference, through the unmodified production catalog.
func outputUpgradeProductionPreparation(t *testing.T, testCase outputUpgradeCase, request task.TaskRecord, reference task.PredicateRef, prepare outputUpgradeCandidateSource) outputUpgradePreparation {
	t.Helper()
	catalog := NativeCatalog()
	candidate, err := prepare(catalog, request, outputUpgradeStoredRecord(t, catalog, testCase, request, reference))
	if err != nil {
		t.Fatalf("stored-predicate candidate under %+v: %v", reference, err)
	}
	return outputUpgradePreparationOf(t, candidate, outputUpgradeAdmit(t, testCase, candidate))
}

// outputUpgradeStoredRecord is the record a task admitted under reference
// persisted. OpenCode references are historical run records; the other
// providers' fresh admission is the launch contract their sources recorded.
func outputUpgradeStoredRecord(t *testing.T, catalog commonprovider.Catalog, testCase outputUpgradeCase, request task.TaskRecord, reference task.PredicateRef) task.MetaRecord {
	t.Helper()
	if testCase.provider == openCodeProvider {
		return tasklifecycle.NewMeta(request, openCodeHistoricalRunProfile(t, catalog, request, testCase.revision, reference), task.SupervisorRef{}, "/runner", task.RunnerOwnershipManaged, outputUpgradeBuild)
	}
	fresh, err := catalog.Candidate(request)
	if err != nil {
		t.Fatalf("current candidate: %v", err)
	}
	seed := tasklifecycle.NewMeta(request, outputUpgradeAdmit(t, testCase, fresh), task.SupervisorRef{}, "/runner", task.RunnerOwnershipManaged, outputUpgradeBuild)
	seed.Predicate = reference
	return seed
}

// outputUpgradePreparationOf projects an admitted candidate onto the
// components an output-only upgrade must leave unchanged.
func outputUpgradePreparationOf(t *testing.T, candidate commonprovider.ProfileCandidate, profile PreparedProfile) outputUpgradePreparation {
	t.Helper()
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	effective, err := task.MarshalCanonical(profile.Effective)
	if err != nil {
		t.Fatal(err)
	}
	plan := profile.Plan
	plan.Predicate = task.PredicateRef{}
	return outputUpgradePreparation{
		plan: plan, effective: effective, writableRoots: profile.WritableRoots,
		preflight: profile.RuntimePreflightSHA256, inspectionDigest: digest, predicate: profile.Plan.Predicate,
	}
}

// outputUpgradeAdmit finalizes a production candidate. Claude admits its static
// runtime-only contract; CCR has no static admission, so the recorded
// launch-help projection finalizes it exactly as a completed inspection would.
func outputUpgradeAdmit(t *testing.T, testCase outputUpgradeCase, candidate commonprovider.ProfileCandidate) PreparedProfile {
	t.Helper()
	if testCase.provider != ccr.Provider {
		profile, err := candidate.Admit()
		if err != nil {
			t.Fatalf("static admission: %v", err)
		}
		return profile
	}
	profile, err := candidate.Finalize(outputUpgradeCCRFacts(t, candidate, testCase.route), time.Now())
	if err != nil {
		t.Fatalf("CCR finalization: %v", err)
	}
	return profile
}

func outputUpgradeCCRFacts(t *testing.T, candidate commonprovider.ProfileCandidate, route bool) []byte {
	t.Helper()
	native, err := task.MarshalCanonical(map[string]bool{"resume_route": route})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture",
	}, native)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// TestCCROutputUpgradeSourcesFailClosedOnCapabilityDisagreement keeps each CCR
// source and target bound to the route capability that selected its launch, for
// reconstruction and for a new successor alike: a route-preserving contract
// refuses a CLI without --resume-route, and a modeled contract refuses a CLI
// that advertises it.
func TestCCROutputUpgradeSourcesFailClosedOnCapabilityDisagreement(t *testing.T) {
	operations := map[string]outputUpgradeCandidateSource{
		"existing":  commonprovider.Catalog.ExistingCandidate,
		"successor": commonprovider.Catalog.SuccessorCandidate,
	}
	for _, testCase := range outputUpgradeCCRCases(t) {
		for _, reference := range []task.PredicateRef{testCase.source, testCase.target} {
			for name, prepare := range operations {
				t.Run(testCase.name+"/"+reference.Version+"/"+name, func(t *testing.T) {
					requireCCRCapabilityDisagreementRefused(t, testCase, reference, prepare)
				})
			}
		}
	}
}

// outputUpgradeCCRCases is the CCR subset of the eligible source cases.
func outputUpgradeCCRCases(t *testing.T) []outputUpgradeCase {
	t.Helper()
	return slices.DeleteFunc(outputUpgradeCases(t), func(testCase outputUpgradeCase) bool { return testCase.provider != ccr.Provider })
}

// requireCCRCapabilityDisagreementRefused finalizes the candidate prepared from
// a record stored under reference: the recorded route capability is accepted
// and the opposite one is refused as an unavailable profile.
func requireCCRCapabilityDisagreementRefused(t *testing.T, testCase outputUpgradeCase, reference task.PredicateRef, prepare outputUpgradeCandidateSource) {
	t.Helper()
	installOutputUpgradeProvider(t, testCase.executable())
	request := outputUpgradeSuccessorRequest(t, testCase)
	catalog := NativeCatalog()
	fresh, err := catalog.Candidate(request)
	if err != nil {
		t.Fatal(err)
	}
	seed := tasklifecycle.NewMeta(request, outputUpgradeAdmit(t, testCase, fresh), task.SupervisorRef{}, "/runner", task.RunnerOwnershipManaged, outputUpgradeBuild)
	seed.Predicate = reference
	candidate, err := prepare(catalog, request, seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.Finalize(outputUpgradeCCRFacts(t, candidate, testCase.route), time.Now()); err != nil {
		t.Fatalf("recorded capability was refused: %v", err)
	}
	if _, err = candidate.Finalize(outputUpgradeCCRFacts(t, candidate, !testCase.route), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Errorf("opposite capability error=%v, want %v", err, commonprovider.ErrProfileUnavailable)
	}
}

// TestCCROutputUpgradeSourcesKeepOriginalContinuationValidator keeps the
// predecessor-owned validator authoritative: a route-preserving predecessor
// refuses a model change and a modeled predecessor still allows an override.
func TestCCROutputUpgradeSourcesKeepOriginalContinuationValidator(t *testing.T) {
	for _, testCase := range outputUpgradeCases(t) {
		if testCase.provider != ccr.Provider {
			continue
		}
		t.Run(testCase.name, func(t *testing.T) {
			predecessor := outputUpgradeSuccessorRequest(t, testCase)
			predecessor.PriorSession = nil
			successor := outputUpgradeSuccessorRequest(t, testCase)
			successor.RequestedConfig.Model = "changed-alias"
			err := NativeCatalog().ValidateContinuation(predecessor, task.MetaRecord{Predicate: testCase.source}, successor)
			if testCase.route && err == nil {
				t.Error("route-preserving predecessor accepted a model change")
			}
			if !testCase.route && err != nil {
				t.Errorf("modeled predecessor refused its allowed override: %v", err)
			}
		})
	}
}
