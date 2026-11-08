package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/pi"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestContinuationWithoutInspectionHeadFollowsPrivateSuccessor(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		history   [][2]string
		published string
	}{
		{name: "A to B", history: [][2]string{{"A", "B"}}, published: "B"},
		{name: "A to B to C", history: [][2]string{{"A", "B"}, {"B", "C"}}, published: "C"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newContinuationSuccessorTestState(t)
			seedContinuationSuccessorTestHistory(t, fixture, testCase.history, testCase.published)
			for _, gate := range []string{"rebind", "exclusive revalidation"} {
				t.Run(gate, func(t *testing.T) {
					assertContinuationSuccessorTestGate(t, fixture, gate, fixture.bindings["A"], fixture.bindings[testCase.published], nil)
				})
			}
		})
	}
}

func TestContinuationWithoutInspectionHeadRevalidatesNewerSuccessor(t *testing.T) {
	fixture := newContinuationSuccessorTestState(t)
	seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
	assertContinuationSuccessorTestGate(t, fixture, "rebind", fixture.bindings["A"], fixture.bindings["B"], nil)

	// A second migration wins between admission's shared observation and its
	// exclusive preparation. The second gate must resolve from B again.
	seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}, {"B", "C"}}, "C")
	assertContinuationSuccessorTestGate(t, fixture, "exclusive revalidation", fixture.bindings["B"], fixture.bindings["C"], nil)
}

func TestContinuationWithoutInspectionHeadRetainsIncompletePublication(t *testing.T) {
	for _, blocker := range []string{"migration journal", "identity publication", "recovery alias"} {
		t.Run(blocker, func(t *testing.T) {
			fixture := newContinuationSuccessorTestState(t)
			seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
			seedContinuationSuccessorTestBlocker(t, fixture, blocker)
			if complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.store.Root, fixture.bindings["B"]); err != nil || complete {
				t.Fatalf("fixture is not a valid incomplete publication: complete=%t err=%v", complete, err)
			}
			for _, gate := range []string{"rebind", "exclusive revalidation"} {
				t.Run(gate, func(t *testing.T) {
					assertContinuationSuccessorTestGate(t, fixture, gate, fixture.bindings["A"], task.SupervisorRef{}, pueue.ErrPrivateSupervisorMigrationRequired)
				})
			}
		})
	}
}

func TestContinueRejectsIncompatibleSuccessorBeforePrivateSupervisorRecovery(t *testing.T) {
	fixture := newContinuationSuccessorTestState(t)
	seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
	seedContinuationSuccessorTestBlocker(t, fixture, "recovery alias")
	predecessor := newContinuationCompatibilityPredecessor(t, fixture, fixture.bindings["A"])
	before := testsupportcore.SnapshotStore(t, fixture.store.Root)

	result := continueTask(Arguments{
		Root: fixture.store.Root, TaskID: predecessor.TaskID,
		Config: task.TaskConfig{Model: "litellm-glm-5-2-successor"},
	}, Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})
	if result.code != 2 || !errors.Is(result.err, commonprovider.ErrUnsupportedOption) {
		t.Fatalf("incompatible continuation result=%+v, want unsupported model override before supervisor recovery", result)
	}
	if result.response.TaskRecord != "not_created" || result.response.TaskID != "" {
		t.Fatalf("incompatible continuation created or exposed a successor: %+v", result.response)
	}
	if !maps.Equal(before, testsupportcore.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("incompatible continuation changed private supervisor state")
	}
	present, err := pueue.PrivateRecoveryBindingPresent(fixture.store.Root)
	if err != nil || !present {
		t.Fatalf("incompatible continuation changed the recovery alias: present=%t err=%v", present, err)
	}
}

func TestContinuationSuccessorPreservesCCRHistoricalModelSelection(t *testing.T) {
	previous := ccrPreviousContinuationReference(t)
	for _, testCase := range ccrContinuationSuccessorCases(previous) {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, owner := newCCRContinuationSuccessorTestState(t)
			seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
			seedContinuationSuccessorTestBlocker(t, fixture, "recovery alias")
			workspace, ccrHome := newCCRContinuationEnvironment(t)
			runCCRContinuationSuccessorCase(t, fixture, owner, workspace, ccrHome, testCase)
		})
	}
}

func TestRunContinuationPreservesCCRHistoricalModelSelection(t *testing.T) {
	previous := ccrPreviousContinuationReference(t)
	testCase := ccrContinuationSuccessorCases(previous)[1]
	for _, entryPoint := range []string{"continue --task", "dispatch --resume"} {
		t.Run(entryPoint, func(t *testing.T) {
			runCCRContinuationEntryPoint(t, entryPoint, testCase)
		})
	}
}

// ccrPreviousContinuationReference selects the registered sealed v2 profile by
// its exact revision; recovery claims belong to the current targets.
func ccrPreviousContinuationReference(t *testing.T) task.PredicateRef {
	t.Helper()
	for _, interpreter := range ccr.Registration().Interpreters {
		if reference := interpreter.Reference(); reference.Mode == config.ModeReadOnly && reference.Version == "ccr-foreground-v2" {
			return reference
		}
	}
	t.Fatal("CCR previous continuation predicate is not registered")
	return task.PredicateRef{}
}

func runCCRContinuationEntryPoint(t *testing.T, entryPoint string, testCase ccrContinuationSuccessorCase) {
	t.Helper()
	fixture, owner := newCCRContinuationSuccessorTestState(t)
	seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
	seedContinuationSuccessorTestBlocker(t, fixture, "recovery alias")
	workspace, ccrHome := newCCRContinuationEnvironment(t)
	predecessor := newCCRContinuationSuccessorPredecessor(t, fixture, workspace, ccrHome, testCase.predicate)

	initialExecutable := filepath.Join(t.TempDir(), "missing-pueue")

	var candidate commonprovider.ProfileCandidate
	catalog := ccrContinuationCatalog(t, &candidate)
	briefPath := filepath.Join(workspace, "follow-up.md")
	if err := os.WriteFile(briefPath, []byte("continue this CCR session"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, wantCode, wantStage := ccrContinuationEntryPointInvocation(t, entryPoint, fixture, workspace, briefPath, predecessor, testCase)

	deps := withSupervisorProbeRouting(t, Dependencies{
		ResourceChecker: fixtureResources,
		Catalog:         catalog, InitialSupervisorExecutable: initialExecutable,
		SupervisorOptions: pueue.Options{ControlOwnerExecutable: canonicalAppTestExecutable(t)},
	}, owner)
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, deps)
	assertCCRContinuationResponse(t, entryPoint, code, &stdout, &stderr, wantCode, wantStage)
	assertCCRContinuationProfile(t, entryPoint, candidate, testCase)
}

func ccrContinuationEntryPointInvocation(t *testing.T, entryPoint string, fixture continuationSuccessorTestState, workspace, briefPath string, predecessor *task.TaskRecord, testCase ccrContinuationSuccessorCase) (args []string, wantCode int, wantStage string) {
	t.Helper()
	args = []string{"--root", fixture.store.Root}
	switch entryPoint {
	case "continue --task":
		args = append(args, "continue", "--task", predecessor.TaskID, "--model", testCase.model, "--json")
		return args, 2, "resolve-predecessor"
	case "dispatch --resume":
		args = append(args, "dispatch", "--provider", ccr.Provider, "--brief", briefPath, "--cwd", workspace,
			"--resume-task", predecessor.TaskID, "--model", testCase.model, "--permission", config.ModeReadOnly,
			"--budget", "1m0s", "--json")
		return args, 2, "resolve-predecessor"
	default:
		t.Fatalf("unknown CCR continuation entrypoint %q", entryPoint)
		return nil, 0, ""
	}
}

func assertCCRContinuationResponse(t *testing.T, entryPoint string, code int, stdout, stderr *bytes.Buffer, wantCode int, wantStage string) {
	t.Helper()
	if code != wantCode || stderr.Len() != 0 {
		t.Fatalf("%s exit=%d stdout=%q stderr=%q", entryPoint, code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Stage != wantStage {
		t.Fatalf("%s changed state before supervisor recovery: %+v", entryPoint, response)
	}
}

func assertCCRContinuationProfile(t *testing.T, entryPoint string, candidate commonprovider.ProfileCandidate, testCase ccrContinuationSuccessorCase) {
	t.Helper()
	if candidate.Finalize == nil {
		t.Fatalf("%s did not reconstruct the recorded CCR candidate", entryPoint)
	}
	facts, err := continuationCCRInspectionFacts(t, candidate, testCase.wantNative)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Plan.Predicate.Equal(testCase.predicate) || !slices.Equal(profile.Plan.Arguments, testCase.wantArgv) {
		t.Fatalf("%s successor plan=%+v want predicate=%+v argv=%q", entryPoint, profile.Plan, testCase.predicate, testCase.wantArgv)
	}
}

type ccrContinuationSuccessorCase struct {
	name       string
	predicate  task.PredicateRef
	model      string
	wantArgv   []string
	wantNative map[string]bool
}

func newCCRContinuationEnvironment(t *testing.T) (workspace, ccrHome string) {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workspace = mustCCRContinuationTempDir(t, userHome, "delegation-ccr-workspace-")
	ccrHome = mustCCRContinuationTempDir(t, userHome, "delegation-ccr-home-")
	ccrPath := filepath.Join(ccrHome, "ccr")
	if err := os.WriteFile(ccrPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", ccrHome)
	t.Setenv("PATH", ccrHome)
	return workspace, ccrHome
}

func mustCCRContinuationTempDir(t *testing.T, parent, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	return dir
}

func ccrContinuationSuccessorCases(previous task.PredicateRef) []ccrContinuationSuccessorCase {
	return []ccrContinuationSuccessorCase{
		{
			name:       "v1 historical model override",
			predicate:  ccr.HistoricalReference(config.ModeReadOnly),
			model:      "legacy-v1-alias",
			wantArgv:   ccrLegacyContinuationArguments("legacy-v1-alias"),
			wantNative: map[string]bool{"forwarded_flags_verified": true},
		},
		{
			name:       "v2 historical model override",
			predicate:  previous,
			model:      "legacy-v2-alias",
			wantArgv:   ccrLegacyContinuationArguments("legacy-v2-alias"),
			wantNative: map[string]bool{"forwarded_flags_verified": true},
		},
		{
			name:       "v3 keeps recorded route without override",
			predicate:  ccr.Reference(config.ModeReadOnly),
			model:      "recorded-v3-alias",
			wantArgv:   ccrRouteContinuationArguments(),
			wantNative: map[string]bool{"resume_route": true},
		},
	}
}

func runCCRContinuationSuccessorCase(t *testing.T, fixture continuationSuccessorTestState, owner *componentSupervisorFixture, workspace, ccrHome string, testCase ccrContinuationSuccessorCase) {
	t.Helper()
	predecessor := newCCRContinuationSuccessorPredecessor(t, fixture, workspace, ccrHome, testCase.predicate)

	transport := installedGenerationCandidate(t, fixture.store, fixture.bindings["B"])

	var candidate commonprovider.ProfileCandidate
	catalog := ccrContinuationCatalog(t, &candidate)
	before := testsupportcore.SnapshotStore(t, fixture.store.Root)
	deps := withSupervisorProbeRouting(t, Dependencies{
		ResourceChecker: fixtureResources,
		Catalog:         catalog, InitialSupervisorExecutable: transport.ClientExecutable,
		SupervisorOptions: pueue.Options{ControlOwnerExecutable: canonicalAppTestExecutable(t)},
	}, owner)
	result := continueTask(Arguments{
		Root: fixture.store.Root, TaskID: predecessor.TaskID,
		Config: task.TaskConfig{Model: testCase.model},
	}, deps)
	if result.code != 1 || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("continuation result=%+v, want blocked supervisor recovery after admission", result)
	}
	if result.response.TaskRecord != "not_created" || task.ValidateTaskID(result.response.TaskID) != nil || result.response.TaskID == predecessor.TaskID {
		t.Fatalf("continuation lost its uncreated successor identity: %+v", result.response)
	}
	failure := dispatchFailure(result.err, result.response.TaskRecord)
	wantFailure := DispatchFailureResponse{Code: "supervisor_upgrade_required", ReasonCode: "historical_target_not_ready", Stage: "resolve-predecessor", NextAction: "stop_and_report"}
	if failure != wantFailure {
		t.Fatalf("continuation lost supervisor recovery guidance: %+v", failure)
	}
	if !maps.Equal(before, testsupportcore.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("continuation changed state-root files before supervisor recovery")
	}
	if candidate.Finalize == nil {
		t.Fatal("continueTask did not prepare a CCR candidate from the recorded predicate")
	}
	facts, err := continuationCCRInspectionFacts(t, candidate, testCase.wantNative)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Plan.Predicate.Equal(testCase.predicate) || !slices.Equal(profile.Plan.Arguments, testCase.wantArgv) {
		t.Fatalf("successor plan=%+v want predicate=%+v argv=%q", profile.Plan, testCase.predicate, testCase.wantArgv)
	}
}

func newCCRContinuationSuccessorPredecessor(t *testing.T, fixture continuationSuccessorTestState, workspace, ccrHome string, predicate task.PredicateRef) *task.TaskRecord {
	t.Helper()
	predecessor := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32),
		Provider: ccr.Provider, Mode: config.ModeReadOnly, CanonicalCwd: workspace,
		RequestedConfig: task.TaskConfig{Model: "recorded-v3-alias", Permission: config.ModeReadOnly, Budget: "1m0s"}, BudgetNanos: int64(time.Minute),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: predecessor.RootID, TaskID: predecessor.TaskID,
		RequestedConfig: predecessor.RequestedConfig,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("ccr continuation fixture"))},
		Containment:     "fixture-only", Approval: "never",
		ProviderExecutable: filepath.Join(ccrHome, "ccr"), ProviderVersion: "fixture-v1",
		RunnerExecutable: "/task/delegate-run", RunnerOwnership: task.RunnerOwnershipManaged,
		PublisherBuild: "fixture-build", PublisherVersion: "fixture-v1",
		Predicate: predicate, Environment: []string{"HOME=" + ccrHome, "PATH=" + ccrHome}, EnvironmentRecorded: true,
		SupervisorConfig: fixture.bindings["A"], CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	brief := []byte("continue this CCR session")
	predecessor.BriefSHA256 = task.ComputeSHA256(brief)
	predecessor.BriefLength = int64(len(brief))
	td, err := fixture.store.CreateTask(predecessor.TaskID, predecessor, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	recordContinuationTestIdentity(t, td, ccr.Provider, "recorded-session")
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	return predecessor
}

func ccrContinuationCatalog(t *testing.T, captured *commonprovider.ProfileCandidate) commonprovider.Catalog {
	t.Helper()
	registration := ccr.Registration()
	capture := func(prepare commonprovider.PrepareExistingCandidate) commonprovider.PrepareExistingCandidate {
		return func(request task.TaskRecord, meta task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			candidate, err := prepare(request, meta, environment)
			if err == nil {
				*captured = candidate
			}
			return candidate, err
		}
	}
	// A normal continuation prepares through the successor hook; reconstruction
	// of an admitted task prepares through the existing hook.
	registration.PrepareExisting = capture(registration.PrepareExisting)
	registration.PrepareSuccessor = capture(registration.PrepareSuccessor)
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func ccrLegacyContinuationArguments(model string) []string {
	return []string{"launch", "--model", model, "--auth-mode", "provider-only", "--print", "--permission-mode", "plan", "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--resume", "recorded-session"}
}

func ccrRouteContinuationArguments() []string {
	return []string{"launch", "--resume-route", "--print", "--permission-mode", "plan", "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--resume", "recorded-session"}
}

func continuationCCRInspectionFacts(t *testing.T, candidate commonprovider.ProfileCandidate, native map[string]bool) ([]byte, error) {
	t.Helper()
	if candidate.Inspection == nil {
		return nil, errors.New("CCR candidate has no inspection definition")
	}
	nativeBytes, err := task.MarshalCanonical(native)
	if err != nil {
		return nil, err
	}
	return commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable,
		SHA256:     candidate.Inspection.ExecutableSHA256,
		Version:    "fixture",
	}, nativeBytes)
}

func TestInvalidPiContinuationOptionDoesNotRecoverPrivateSupervisor(t *testing.T) {
	// Unknown well-formed levels pass through; an option-like literal is invalid.
	for _, testCase := range []struct {
		name string
		run  func(continuationSuccessorTestState, *task.TaskRecord, Dependencies) commandResult
	}{
		{
			name: "continue",
			run: func(fixture continuationSuccessorTestState, predecessor *task.TaskRecord, deps Dependencies) commandResult {
				return continueTask(Arguments{
					Root: fixture.store.Root, TaskID: predecessor.TaskID,
					Config: task.TaskConfig{Effort: "--tools"},
				}, deps)
			},
		},
		{
			name: "dispatch resume",
			run: func(fixture continuationSuccessorTestState, predecessor *task.TaskRecord, deps Dependencies) commandResult {
				return dispatch(Arguments{
					Root: fixture.store.Root, Provider: pi.Provider, Cwd: predecessor.CanonicalCwd,
					BriefBytes: []byte("continue the exact Pi session"), ResumeTask: predecessor.TaskID,
					Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m", Effort: "--tools"},
				}, deps)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newContinuationSuccessorTestState(t)
			seedContinuationSuccessorTestHistory(t, fixture, [][2]string{{"A", "B"}}, "B")
			seedContinuationSuccessorTestBlocker(t, fixture, "recovery alias")
			predecessor := newPiContinuationPredecessor(t, fixture, fixture.bindings["A"])
			before := testsupportcore.SnapshotStore(t, fixture.store.Root)
			result := testCase.run(fixture, predecessor, Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})

			if result.code != 2 || !errors.Is(result.err, pi.ErrUnsupportedProfile) {
				t.Fatalf("invalid Pi continuation result=%+v, want malformed effort rejection before supervisor recovery", result)
			}
			if result.response.TaskRecord != "not_created" {
				t.Fatalf("invalid Pi continuation created a successor: %+v", result.response)
			}
			if testCase.name == "continue" && result.response.TaskID != "" {
				t.Fatalf("continue exposed an uncreated successor: %+v", result.response)
			}
			if !maps.Equal(before, testsupportcore.SnapshotStore(t, fixture.store.Root)) {
				t.Fatal("invalid Pi continuation changed private supervisor state")
			}
			present, err := pueue.PrivateRecoveryBindingPresent(fixture.store.Root)
			if err != nil || !present {
				t.Fatalf("invalid Pi continuation changed the recovery alias: present=%t err=%v", present, err)
			}
		})
	}
}

type continuationSuccessorTestState struct {
	store    *taskdir.Store
	bindings map[string]task.SupervisorRef
}

func newContinuationCompatibilityPredecessor(t *testing.T, fixture continuationSuccessorTestState, supervisor task.SupervisorRef) *task.TaskRecord {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	brief := []byte("continue this exact CCR session")
	requested := task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m0s", Model: "litellm-glm-5-2"}
	digest := task.ComputeSHA256([]byte("continuation compatibility"))
	request := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, TaskID: id,
		Provider: ccr.Provider, Mode: config.ModeReadOnly, CanonicalCwd: workspace,
		RequestedConfig: requested, BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, TaskID: id,
		RequestedConfig: requested,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment:     "fixture-only", Approval: "never",
		ProviderExecutable: filepath.Join(fixture.store.Root, "ccr"), ProviderVersion: "fixture-v1",
		RunnerExecutable: filepath.Join(fixture.store.Root, "delegate-run"), RunnerOwnership: task.RunnerOwnershipManaged,
		PublisherBuild: "fixture-build", PublisherVersion: "fixture-v1",
		Environment: []string{"HOME=" + filepath.Join(fixture.store.Root, "home")}, EnvironmentRecorded: true,
		Predicate: ccr.Reference(config.ModeReadOnly), SupervisorConfig: supervisor,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := fixture.store.CreateTask(id, request, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	recordContinuationTestIdentity(t, td, request.Provider, "ccr-continuation-session")
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	return request
}

func newPiContinuationPredecessor(t *testing.T, fixture continuationSuccessorTestState, supervisor task.SupervisorRef) *task.TaskRecord {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err = os.MkdirAll(filepath.Join(home, ".pi", "agent", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const sessionID = "11111111-1111-4111-8111-111111111111"
	brief := []byte("continue this exact Pi session")
	requested := task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m0s"}
	digest := task.ComputeSHA256([]byte("Pi continuation compatibility"))
	request := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, TaskID: id,
		Provider: pi.Provider, Mode: config.ModeReadOnly, CanonicalCwd: workspace,
		RequestedConfig: requested, BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, TaskID: id,
		RequestedConfig: requested,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment:     "fixture-only", Approval: "never",
		ProviderExecutable: filepath.Join(fixture.store.Root, "pi"), ProviderVersion: "fixture-v1",
		RunnerExecutable: filepath.Join(fixture.store.Root, "delegate-run"), RunnerOwnership: task.RunnerOwnershipManaged,
		PublisherBuild: "fixture-build", PublisherVersion: "fixture-v1",
		Environment: []string{"HOME=" + home, "PATH=/usr/bin:/bin"}, EnvironmentRecorded: true,
		Predicate: pi.Reference(), SupervisorConfig: supervisor,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := fixture.store.CreateTask(id, request, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	recordContinuationTestIdentity(t, td, request.Provider, sessionID)
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	return request
}

func recordContinuationTestIdentity(t *testing.T, td *taskdir.TaskDir, provider, conversationID string) {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := historicalConsumeSubmissionPermit(permit)
	if err != nil {
		t.Fatal(err)
	}
	id := int64(7)
	observation := pueue.Observation{
		Matched: true, State: pueue.StateRunning, Binding: meta.SupervisorConfig, NumericTaskID: id,
		Identity: pueue.Identity{
			RootID: submission.RootID, TaskID: submission.TaskID,
			SpecSHA256: submission.SpecSHA256, MetaSHA256: submission.MetaSHA256, NumericTaskID: &id,
		},
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: provider, ConversationID: conversationID}); err != nil {
		t.Fatal(err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
}

func assertContinuationSuccessorTestGate(t *testing.T, fixture continuationSuccessorTestState, gate string, saved, want task.SupervisorRef, wantErr error) {
	t.Helper()
	assertSupervisorProbeTestNoAdmissionAuthority(t, fixture.store)
	before := testsupportcore.SnapshotStore(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var got task.SupervisorRef
	var err error
	switch gate {
	case "rebind":
		got, err = supervisorcore.RebindContinuation(ctx, fixture.store.Root, saved, continuationSupervisorOptions(Dependencies{ResourceChecker: fixtureResources}))
	case "exclusive revalidation":
		// Exercise the actual exclusive owner, including its recovery-alias
		// handling, rather than invoking the revalidation helper in isolation.
		got, err = supervisorcore.PrepareContinuationSuccessor(ctx, fixture.store.Root, saved, continuationSupervisorOptions(Dependencies{ResourceChecker: fixtureResources}))
	default:
		t.Fatalf("unknown continuation gate %q", gate)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("%s returned error %v, want %v", gate, err, wantErr)
	}
	if wantErr == nil && got != want {
		t.Errorf("%s did not follow the published chain: got=%+v want=%+v", gate, got, want)
	}
	if wantErr != nil && got != (task.SupervisorRef{}) && got != saved {
		t.Errorf("%s exposed a successor before publication completed: %+v", gate, got)
	}
	if !maps.Equal(before, testsupportcore.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("continuation gate changed state-root files")
	}
	assertSupervisorProbeTestNoAdmissionAuthority(t, fixture.store)
}

func newContinuationSuccessorTestState(t *testing.T) continuationSuccessorTestState {
	t.Helper()
	store := openContinuationSuccessorTestStoreAt(t, filepath.Join(testsupportcore.CanonicalTempDir(t), "state"))
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return newContinuationSuccessorTestStateWithStore(t, store)
}

func newCCRContinuationSuccessorTestState(t *testing.T) (continuationSuccessorTestState, *componentSupervisorFixture) {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	owner := newComponentSupervisorFixtureIn(t, componentFiniteDaemon, userHome)
	store := openContinuationSuccessorTestStoreAt(t, owner.root)
	owner.ownStore(t, store)
	return newContinuationSuccessorTestStateWithStore(t, store), owner
}

func newContinuationSuccessorTestStateWithStore(t *testing.T, store *taskdir.Store) continuationSuccessorTestState {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	// Native binding creates these configured directories before publication.
	for _, directory := range []string{"data", "run"} {
		if mkdirErr := os.MkdirAll(filepath.Join(base, directory), 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	writeContinuationSuccessorTestBytes(t, filepath.Join(base, "bootstrap.lock"), nil)
	fixture := continuationSuccessorTestState{store: store, bindings: make(map[string]task.SupervisorRef)}
	for _, name := range []string{"A", "B", "C"} {
		digest := task.ComputeSHA256([]byte("generation " + name))
		binding := testsupport.ContinuationSupervisorRef(store.Root, digest, "unix:"+filepath.Join(base, "run", name+".sock"))
		binding.ConfigDigest = task.ComputeSHA256(continuationSuccessorTestConfig(t, store.Root, name))
		// A no-head continuation must use the published records even after the
		// predecessor installation disappears. Neither executable exists.
		binding.ClientExecutable = filepath.Join(base, "removed-"+name, "pueue")
		binding.DaemonExecutable = filepath.Join(base, "removed-"+name, "pueued")
		fixture.bindings[name] = binding
	}
	return fixture
}

func openContinuationSuccessorTestStoreAt(t *testing.T, root string) *taskdir.Store {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".probe"), 0o700); err != nil {
		t.Fatal(err)
	}
	// These tests start with a published root. Seed its immutable record and
	// lock directly; store initialization and staging are outside this scope.
	writeContinuationSuccessorTestRecord(t, filepath.Join(root, "root.json"), task.RootRecord{
		SchemaVersion: task.SchemaVersion, RootID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: "2026-10-09T00:00:00Z",
	})
	writeContinuationSuccessorTestBytes(t, filepath.Join(root, ".maintenance.lock"), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store, err := openStoreContext(ctx, root, Dependencies{ResourceChecker: fixtureResources}.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func seedContinuationSuccessorTestHistory(t *testing.T, fixture continuationSuccessorTestState, edges [][2]string, published string) {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(fixture.store.Root))
	migrations := make([]map[string]any, 0, len(edges))
	for index, edge := range edges {
		migration := continuationSuccessorTestMigration(fixture.bindings[edge[0]], fixture.bindings[edge[1]])
		migration["legacy_pid"] = 101 + index
		migrations = append(migrations, migration)
	}
	// These envelopes mirror the private package's persisted history and
	// publication records. They deliberately create no inspection group head.
	writeContinuationSuccessorTestRecord(t, filepath.Join(base, "migration-history.json"), map[string]any{
		"schema_version": task.SchemaVersion, "migrations": migrations,
	})
	current := fixture.bindings[published]
	writeContinuationSuccessorTestRecord(t, filepath.Join(base, "supervisor.identity.json"), map[string]any{
		"schema_version": task.SchemaVersion, "binding": current,
	})
	writeContinuationSuccessorTestRecord(t, filepath.Join(base, "daemon.identity.json"), map[string]any{
		"executable": current.DaemonExecutable, "sha256": current.DaemonSHA256,
	})
	writeContinuationSuccessorTestBytes(t, current.ConfigPath, continuationSuccessorTestConfig(t, fixture.store.Root, published))
	if complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.store.Root, current); err != nil || !complete {
		t.Fatalf("published fixture is incomplete: complete=%t err=%v", complete, err)
	}
}

func continuationSuccessorTestMigration(legacy, successor task.SupervisorRef) map[string]any {
	return map[string]any{
		"schema_version": task.SchemaVersion, "legacy_pid": 101, "legacy_stopped": true,
		"legacy": continuationSuccessorTestIdentity(legacy), "successor": continuationSuccessorTestIdentity(successor),
	}
}

func continuationSuccessorTestIdentity(binding task.SupervisorRef) map[string]any {
	return map[string]any{
		"executable": binding.DaemonExecutable, "sha256": binding.DaemonSHA256,
		"config_path": binding.ConfigPath, "config_digest": binding.ConfigDigest,
		"resolved_config_sha256": binding.ResolvedConfigSHA256, "endpoint": binding.Endpoint,
	}
}

func seedContinuationSuccessorTestBlocker(t *testing.T, fixture continuationSuccessorTestState, blocker string) {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(fixture.store.Root))
	switch blocker {
	case "migration journal":
		writeContinuationSuccessorTestRecord(t, filepath.Join(base, "migration.json"), continuationSuccessorTestMigration(fixture.bindings["A"], fixture.bindings["B"]))
	case "identity publication":
		writeContinuationSuccessorTestRecord(t, filepath.Join(base, "identity.pending.json"), map[string]any{
			"schema_version": task.SchemaVersion, "binding": fixture.bindings["B"],
		})
	case "recovery alias":
		config := continuationSuccessorTestConfig(t, fixture.store.Root, "A")
		configPath := filepath.Join(base, "legacy-pueue.yml")
		writeContinuationSuccessorTestBytes(t, configPath, config)
		writeContinuationSuccessorTestRecord(t, filepath.Join(base, "legacy-supervisor.json"), map[string]any{
			"schema_version": task.SchemaVersion, "supervisor": fixture.bindings["A"],
			"config_path": configPath, "config_sha256": task.ComputeSHA256(config),
		})
	default:
		t.Fatalf("unknown publication blocker %q", blocker)
	}
}

func continuationSuccessorTestConfig(t *testing.T, root, name string) []byte {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(root))
	data, err := task.MarshalCanonical(map[string]any{
		"shared": map[string]any{
			"pueue_directory":         filepath.Join(base, "data"),
			"runtime_directory":       filepath.Join(base, "run"),
			"alias_file":              filepath.Join(base, "aliases.yml"),
			"use_unix_socket":         true,
			"unix_socket_path":        filepath.Join(base, "run", name+".sock"),
			"unix_socket_permissions": 448,
			"pid_path":                filepath.Join(base, "run", "pueue.pid"),
			"shared_secret_path":      filepath.Join(base, "data", "shared_secret"),
			"daemon_cert":             filepath.Join(base, "data", "certs", "daemon.cert"),
			"daemon_key":              filepath.Join(base, "data", "certs", "daemon.key"),
		},
		"daemon": map[string]any{"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeContinuationSuccessorTestRecord(t *testing.T, path string, record any) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	writeContinuationSuccessorTestBytes(t, path, data)
}

func writeContinuationSuccessorTestBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
