package app

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// workerSuccessorFixture is one state root with a sealed-shape predecessor
// record and a pending prospective successor inspection that the managed
// detached worker consumes through its production operation path.
type workerSuccessorFixture struct {
	store      *taskdir.Store
	workspace  string
	cliDir     string
	nativeLog  string
	supervisor task.SupervisorRef
	pueueEnv   []string
}

const workerSuccessorCLI = `#!/bin/sh
printf '%s\n' "$*" >> "$(dirname "$0")/native.log"
case "$1" in
  version|--version) printf '%s\n' '1.0.0' ;;
  *) printf '%s\n' 'Usage: --model --auth-mode --print --permission-mode --input-format --output-format --verbose --permission-prompts --session-id --resume --resume-route --effort' ;;
esac
`

func newWorkerSuccessorFixture(t *testing.T) *workerSuccessorFixture {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Provider writable roots include /tmp, and placement rejects a state root
	// inside them, so the fixture lives under the user's home.
	base, err := filepath.EvalSymlinks(mustCCRContinuationTempDir(t, userHome, ".delegate-inspection-successor-"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &workerSuccessorFixture{workspace: filepath.Join(base, "workspace"), cliDir: filepath.Join(base, "cli")}
	fixture.nativeLog = filepath.Join(fixture.cliDir, "native.log")
	for _, dir := range []string{fixture.workspace, fixture.cliDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, binary := range []string{"ccr", "claude"} {
		writeRunnerFixture(t, filepath.Join(fixture.cliDir, binary), workerSuccessorCLI)
	}
	t.Setenv("HOME", fixture.cliDir)
	t.Setenv("PATH", fixture.cliDir)
	fixture.store, err = openStore(filepath.Join(base, "state"), Dependencies{ResourceChecker: fixtureResources, Catalog: NativeCatalog()}.storeDependencies(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := fixture.store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	bound, logPath, _ := newUncertainGroupSupervisorWithConfig(t, fixture.store.RootID, pueue.PrivateConfigPath(fixture.store.Root))
	fixture.supervisor = bound.Binding()
	if err = bound.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.pueueEnv = append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+filepath.Join(filepath.Dir(logPath), "status.json"))
	return fixture
}

// workerSuccessorCase names one predecessor record family. native marks a
// provider-owned native permission profile.
type workerSuccessorCase struct {
	name, provider, version, session string
	native                           bool
}

// createPredecessor records one predecessor under a saved scope that the
// successor must never inherit.
func (f *workerSuccessorFixture) createPredecessor(t *testing.T, id string, testCase workerSuccessorCase) task.TaskRecord {
	t.Helper()
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: f.store.RootID, TaskID: id,
		Provider: testCase.provider, Mode: config.ModeReadOnly, CanonicalCwd: f.workspace,
		RequestedConfig: task.TaskConfig{Model: "recorded-alias", Permission: config.ModeReadOnly, Budget: "1m0s"}, BudgetNanos: int64(time.Minute),
	}
	f.createTask(t, request, []byte("predecessor brief"), testCase, []string{"HOME=/saved-home", "PATH=/saved/bin"})
	return request
}

// createTask records one admitted task's prepared request and metadata with
// the given recorded launch scope.
func (f *workerSuccessorFixture) createTask(t *testing.T, request task.TaskRecord, brief []byte, testCase workerSuccessorCase, environment []string) {
	t.Helper()
	meta := task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID, RequestedConfig: request.RequestedConfig,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("successor worker fixture"))},
		Containment:     "fixture-only", Approval: "never",
		ProviderExecutable: "/saved/bin/" + testCase.provider, ProviderVersion: "fixture-v1",
		RunnerExecutable: "/task/delegate-run", RunnerOwnership: task.RunnerOwnershipManaged,
		PublisherBuild: "fixture-build", PublisherVersion: "fixture-v1",
		Predicate: registeredReadOnlyReference(t, testCase.provider, testCase.version), Environment: environment, EnvironmentRecorded: true,
		SupervisorConfig: f.supervisor, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if testCase.native {
		effective, err := commonprovider.NativeEffectiveConfig(request, []string{f.workspace}, strings.Repeat("e", 64), "plan")
		if err != nil {
			t.Fatal(err)
		}
		meta.EffectiveConfig, meta.Containment, meta.Approval = effective, effective.Containment, effective.Approval
	}
	request.BriefSHA256, request.BriefLength = task.ComputeSHA256(brief), int64(len(brief))
	td, err := f.store.CreateTask(request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f *workerSuccessorFixture) successorRequest(t *testing.T, predecessor task.TaskRecord, session string) task.TaskRecord {
	t.Helper()
	prior := &task.PriorSession{Provider: predecessor.Provider, ConversationID: session, PredecessorTaskID: predecessor.TaskID}
	request, err := admission.BuildRequest(f.store.RootID, strings.Repeat("c", 32), predecessor.Provider, f.workspace, task.TaskConfig{
		Model: "recorded-alias", Permission: config.ModeReadOnly, Budget: "1m0s",
	}, []byte("successor brief"), prior)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// queueInspection opens the pending inspection exactly as admission binds it:
// the candidate's definition, digest, and filtered launch scope become the
// immutable binding, and the managed add receipt is already published.
func (f *workerSuccessorFixture) queueInspection(t *testing.T, request task.TaskRecord, candidate commonprovider.ProfileCandidate) {
	t.Helper()
	definition, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	capability := ""
	if definition.RuntimeOnly() {
		if capability, err = commonprovider.RuntimeCapabilityDigest(definition); err != nil {
			t.Fatal(err)
		}
	}
	worker := canonicalAppTestExecutable(t)
	workerSHA, err := commonprovider.FingerprintExecutable(worker)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.OpenOperation(f.store, request, inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: digest, CapabilitySHA256: capability,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: worker, WorkerSHA256: workerSHA, RunnerOwnership: task.RunnerOwnershipManaged,
		Environment: definition.Environment, EnvironmentRecorded: true, Supervisor: f.supervisor,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
}

// runWorker enters the managed supervised worker with an ambient scope that
// cannot locate the provider, so only the binding's own scope can succeed.
func (f *workerSuccessorFixture) runWorker(t *testing.T, taskID string, deps Dependencies) error {
	t.Helper()
	deps.Environment = []string{"HOME=/ambient-home", "PATH=/ambient/bin"}
	deps.SupervisorOptions = pueue.Options{Environment: f.pueueEnv, ObservationTimeout: time.Second}
	return runInspectionWorkerOperationModeWithLifetime(f.store.Root, taskID, true, nil, deps.normalized(), f.store)
}

func (f *workerSuccessorFixture) result(t *testing.T, taskID string) inspection.ResultRecord {
	t.Helper()
	control, err := f.store.OpenInspection(taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	var result inspection.ResultRecord
	readErr := control.Read("result.json", &result)
	if closeErr := control.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return result
}

func (f *workerSuccessorFixture) nativeCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.nativeLog)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// registeredReadOnlyReference selects the first registered read-only
// reference with this exact version from the production catalog.
func registeredReadOnlyReference(t *testing.T, provider, version string) task.PredicateRef {
	t.Helper()
	registration, err := NativeCatalog().Lookup(provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, interpreter := range registration.Interpreters {
		if reference := interpreter.Reference(); reference.Mode == config.ModeReadOnly && reference.Version == version {
			return reference
		}
	}
	t.Fatalf("%s predicate %s is not registered", provider, version)
	return task.PredicateRef{}
}

const workerSuccessorClaudeSession = "11111111-1111-4111-8111-111111111111"

// workerSuccessorCases are the prospective continuations whose admission
// factory differs from the generic current one (legacy CCR and historical
// native Claude), with a current record of each provider as control.
func workerSuccessorCases() []workerSuccessorCase {
	return []workerSuccessorCase{
		{name: "ccr legacy v1", provider: ccr.Provider, version: "ccr-foreground-v1", session: "recorded-session"},
		{name: "ccr legacy v2", provider: ccr.Provider, version: "ccr-foreground-v2", session: "recorded-session"},
		{name: "ccr modern route control", provider: ccr.Provider, version: ccr.Reference(config.ModeReadOnly).Version, session: "recorded-session"},
		{name: "claude historical native v1", provider: claude.Provider, version: "native-permissions-v1", session: workerSuccessorClaudeSession, native: true},
		{name: "claude current native control", provider: claude.Provider, version: claude.ReferenceForMode(config.ModeReadOnly).Version, session: workerSuccessorClaudeSession, native: true},
	}
}

// TestInspectionWorkerCompilesProspectiveSuccessorWithItsAdmissionFactory
// drives the detached worker's production operation path for a prospective
// CCR continuation. Admission compiled the binding through the successor
// factory from the real predecessor record; the worker must compile the same
// definition in the binding's scope, or the strict candidate gate refuses it.
func TestInspectionWorkerCompilesProspectiveSuccessorWithItsAdmissionFactory(t *testing.T) {
	for _, testCase := range workerSuccessorCases() {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newWorkerSuccessorFixture(t)
			predecessor := fixture.createPredecessor(t, strings.Repeat("b", 32), testCase)
			_, predecessorMeta := loadWorkerSuccessorRecords(t, fixture.store, predecessor.TaskID)
			request := fixture.successorRequest(t, predecessor, testCase.session)
			candidate, err := NativeCatalog().SuccessorCandidate(request, *predecessorMeta, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			fixture.queueInspection(t, request, candidate)
			before := readImmutableRecords(t, filepath.Join(fixture.store.Root, "tasks", predecessor.TaskID))

			if err = fixture.runWorker(t, request.TaskID, Dependencies{ResourceChecker: fixtureResources}); err != nil {
				t.Fatalf("worker refused its admitted successor binding: stage=%s", inspectionWorkerFailureStage(err, "unknown"))
			}
			if result := fixture.result(t, request.TaskID); result.Reason != inspection.ResultEligible {
				t.Fatalf("worker result = %s, want eligible", result.Reason)
			}
			if after := readImmutableRecords(t, filepath.Join(fixture.store.Root, "tasks", predecessor.TaskID)); !maps.EqualFunc(after, before, bytes.Equal) {
				t.Fatal("worker changed the predecessor's records")
			}
		})
	}
}

func loadWorkerSuccessorRecords(t *testing.T, store *taskdir.Store, id string) (*task.TaskRecord, *task.MetaRecord) {
	t.Helper()
	td, err := store.OpenTask(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	request, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	return request, meta
}

func modernCCRCase() workerSuccessorCase {
	return workerSuccessorCase{name: "ccr modern", provider: ccr.Provider, version: ccr.Reference(config.ModeReadOnly).Version, session: "recorded-session"}
}

// TestInspectionWorkerRefusesUnreadablePredecessorWithoutSubstitute binds a
// modern CCR successor, whose definition the generic factory would also
// produce, to an immediate predecessor that is missing, malformed, or of
// another provider. The worker refuses before any native inspection instead
// of substituting the generic factory, and a replay launches nothing more.
func TestInspectionWorkerRefusesUnreadablePredecessorWithoutSubstitute(t *testing.T) {
	for _, damage := range []string{"missing", "malformed", "wrong provider"} {
		t.Run(damage, func(t *testing.T) {
			fixture := newWorkerSuccessorFixture(t)
			source := fixture.createPredecessor(t, strings.Repeat("b", 32), modernCCRCase())
			_, sourceMeta := loadWorkerSuccessorRecords(t, fixture.store, source.TaskID)
			request := fixture.successorRequest(t, fixture.damagedPredecessor(t, damage), "recorded-session")
			candidate, err := NativeCatalog().SuccessorCandidate(request, *sourceMeta, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			fixture.queueInspection(t, request, candidate)
			for attempt := range 2 {
				if err = fixture.runWorker(t, request.TaskID, Dependencies{ResourceChecker: fixtureResources}); err == nil {
					t.Fatalf("attempt %d accepted an unreadable predecessor", attempt)
				}
			}
			if got := fixture.result(t, request.TaskID).Reason; got == inspection.ResultEligible {
				t.Fatalf("unreadable predecessor produced result %s", got)
			}
			if calls := fixture.nativeCalls(t); calls != 0 {
				t.Fatalf("refused successor inspection ran %d native commands", calls)
			}
		})
	}
}

// damagedPredecessor returns the immediate predecessor reference for one
// damage kind: an absent task, a task whose metadata is malformed, or a valid
// task of another provider.
func (f *workerSuccessorFixture) damagedPredecessor(t *testing.T, damage string) task.TaskRecord {
	t.Helper()
	predecessor := task.TaskRecord{Provider: ccr.Provider, TaskID: strings.Repeat("d", 32)}
	switch damage {
	case "malformed":
		predecessor = f.createPredecessor(t, predecessor.TaskID, modernCCRCase())
		metaPath := filepath.Join(f.store.Root, "tasks", predecessor.TaskID, "meta.json")
		if err := os.Chmod(metaPath, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metaPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "wrong provider":
		f.createPredecessor(t, predecessor.TaskID, workerSuccessorCases()[4])
	}
	return predecessor
}

// TestInspectionWorkerKeepsAdmittedAndInjectedSelection proves the paths that
// never read the predecessor: an admitted successor reconstructs from its own
// record although its predecessor is absent, and an injected factory keeps
// its own preparation.
func TestInspectionWorkerKeepsAdmittedAndInjectedSelection(t *testing.T) {
	absent := task.TaskRecord{Provider: ccr.Provider, TaskID: strings.Repeat("d", 32)}
	t.Run("admitted successor", func(t *testing.T) {
		fixture := newWorkerSuccessorFixture(t)
		request := fixture.successorRequest(t, absent, "recorded-session")
		fixture.createTask(t, request, []byte("successor brief"), modernCCRCase(), []string{"HOME=" + fixture.cliDir, "PATH=" + fixture.cliDir})
		_, meta := loadWorkerSuccessorRecords(t, fixture.store, request.TaskID)
		candidate, err := NativeCatalog().ExistingCandidate(request, *meta, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		fixture.queueInspection(t, request, candidate)
		if err = fixture.runWorker(t, request.TaskID, Dependencies{ResourceChecker: fixtureResources}); err != nil {
			t.Fatalf("admitted successor needed its predecessor: stage=%s", inspectionWorkerFailureStage(err, "unknown"))
		}
	})
	t.Run("injected factory", func(t *testing.T) {
		fixture := newWorkerSuccessorFixture(t)
		request := fixture.successorRequest(t, absent, "recorded-session")
		candidate, err := NativeCatalog().Candidate(request, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		fixture.queueInspection(t, request, candidate)
		calls := 0
		deps := Dependencies{ResourceChecker: fixtureResources, PrepareCandidate: func(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			calls++
			return NativeCatalog().Candidate(request, environment)
		}}
		if err = fixture.runWorker(t, request.TaskID, deps); err != nil || calls != 1 {
			t.Fatalf("injected factory calls=%d stage=%s", calls, inspectionWorkerFailureStage(err, "none"))
		}
	})
}

// TestInspectionCandidateDependenciesReadPredecessorOnlyForProspectiveSuccessor
// covers the selections that must not open a predecessor at all.
func TestInspectionCandidateDependenciesReadPredecessorOnlyForProspectiveSuccessor(t *testing.T) {
	fixture := newWorkerSuccessorFixture(t)
	absent := task.TaskRecord{Provider: ccr.Provider, TaskID: strings.Repeat("d", 32)}
	prospective := fixture.successorRequest(t, absent, "recorded-session")
	interactive := prospective
	interactive.RequestedConfig.Interactive = true
	fresh := prospective
	fresh.PriorSession = nil
	production := Dependencies{ResourceChecker: fixtureResources, Environment: os.Environ()}.normalized()
	for name, request := range map[string]task.TaskRecord{"interactive continuation": interactive, "fresh or models task": fresh} {
		if _, err := inspectionCandidateDependencies(production, fixture.store, request, nil); err != nil {
			t.Fatalf("%s opened a predecessor: %v", name, err)
		}
	}
	if _, err := inspectionCandidateDependencies(production, fixture.store, prospective, &task.MetaRecord{}); err != nil {
		t.Fatalf("admitted successor opened its predecessor: %v", err)
	}
	if _, err := inspectionCandidateDependencies(production, fixture.store, prospective, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prospective successor without predecessor = %v, want the store's absence", err)
	}
}

// TestCatalogExistingClassifiesAdmittedInteractiveContinuation proves that an
// already admitted interactive continuation reconstructs from its own record,
// while a prospective one still prepares fresh and a custom preparer keeps
// its override.
func TestCatalogExistingClassifiesAdmittedInteractiveContinuation(t *testing.T) {
	faults := map[string]error{"fresh": errors.New("fresh"), "existing": errors.New("existing"), "successor": errors.New("successor")}
	deps := continuationDependencies(Dependencies{ResourceChecker: fixtureResources, Catalog: candidateFactoryCatalog(t, faults)}, task.MetaRecord{})
	request := task.TaskRecord{
		Provider: "fixture:preflight", Mode: config.ModeReadOnly, CanonicalCwd: t.TempDir(),
		PriorSession:    &task.PriorSession{Provider: "fixture:preflight", ConversationID: "known", PredecessorTaskID: "predecessor"},
		RequestedConfig: task.TaskConfig{Permission: config.ModeReadOnly, Interactive: true},
	}
	ordinary := request
	ordinary.RequestedConfig.Interactive = false
	custom := errors.New("custom")
	customDeps := Dependencies{ResourceChecker: fixtureResources, Catalog: deps.Catalog, PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		return PreparedProfile{}, custom
	}}
	for _, test := range []struct {
		name       string
		deps       Dependencies
		request    task.TaskRecord
		meta       *task.MetaRecord
		want       error
		historical bool
	}{
		{name: "prospective interactive", deps: deps, request: request, want: faults["fresh"]},
		{name: "admitted interactive", deps: deps, request: request, meta: &task.MetaRecord{}, want: faults["existing"], historical: true},
		{name: "prospective ordinary", deps: deps, request: ordinary, want: faults["successor"]},
		{name: "admitted ordinary", deps: deps, request: ordinary, meta: &task.MetaRecord{}, want: faults["existing"], historical: true},
		{name: "custom profile admitted interactive", deps: customDeps, request: request, meta: &task.MetaRecord{}, want: custom},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, historical, err := admissionCandidateSource{dependencies: test.deps}.ForTask(test.request, test.meta, os.Environ())
			if !errors.Is(err, test.want) || historical != test.historical {
				t.Fatalf("selection = %v historical=%t, want %v historical=%t", err, historical, test.want, test.historical)
			}
		})
	}
}

// TestProspectiveSuccessorDefinitionRoundTripsThroughFilteredBinding covers
// every production registration that launches and continues. For each
// registered read-only predecessor contract, with the caller's default and
// explicit selectors, admission compiles the successor in the caller's full
// scope; the worker recompiles it from the binding's filtered scope through
// the same successor factory and must reach the identical definition.
func TestProspectiveSuccessorDefinitionRoundTripsThroughFilteredBinding(t *testing.T) {
	fixture := newWorkerSuccessorFixture(t)
	for _, binary := range []string{"agy", "codex", "pi", "opencode"} {
		writeRunnerFixture(t, filepath.Join(fixture.cliDir, binary), workerSuccessorCLI)
	}
	selectors := filepath.Join(fixture.cliDir, "selectors")
	scopes := map[string][]string{
		"default selectors": {"HOME=" + fixture.cliDir, "PATH=" + fixture.cliDir, "DELEGATE_UNRELATED=caller"},
		"explicit selectors": {
			"HOME=" + fixture.cliDir, "PATH=" + fixture.cliDir, "CODEX_HOME=" + filepath.Join(selectors, "codex"),
			"XDG_CONFIG_HOME=" + filepath.Join(selectors, "config"), "XDG_DATA_HOME=" + filepath.Join(selectors, "data"), "TMPDIR=" + filepath.Join(selectors, "tmp"),
		},
	}
	catalog := NativeCatalog()
	for _, description := range catalog.Descriptions() {
		registration, err := catalog.Lookup(description.ID)
		if err != nil {
			t.Fatal(err)
		}
		if registration.Prepare == nil || description.Continuation == commonprovider.ContinuationUnsupported {
			continue
		}
		for scopeName, scope := range scopes {
			if roundTripRegistration(t, fixture, registration, scope) == 0 {
				t.Errorf("%s prepared no successor with %s", description.ID, scopeName)
			}
		}
	}
}

// roundTripRegistration round-trips every registered read-only predecessor
// contract of one registration, native and not, and counts admitted ones.
func roundTripRegistration(t *testing.T, fixture *workerSuccessorFixture, registration commonprovider.Registration, scope []string) int {
	t.Helper()
	admitted := 0
	for _, interpreter := range registration.Interpreters {
		reference := interpreter.Reference()
		if reference.Mode != config.ModeReadOnly {
			continue
		}
		for _, native := range []bool{true, false} {
			if roundTripSuccessorDefinition(t, fixture, registration.Description, reference, native, scope) {
				admitted++
			}
		}
	}
	return admitted
}

// roundTripSuccessorDefinition reports false when the provider refuses this
// predecessor contract at admission; an admitted definition must round-trip.
func roundTripSuccessorDefinition(t *testing.T, fixture *workerSuccessorFixture, description commonprovider.Description, reference task.PredicateRef, native bool, scope []string) bool {
	t.Helper()
	session := "123e4567-e89b-12d3-a456-426614174000"
	if description.ID == config.ProviderOpenCodeRun {
		session = outputUpgradeOpenCodeSession
	}
	requested := task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m0s"}
	if slices.Contains(description.SupportedOptions, commonprovider.OptionModel) {
		requested.Model = "recorded-alias"
	}
	prior := &task.PriorSession{Provider: description.ID, ConversationID: session, PredecessorTaskID: strings.Repeat("b", 32)}
	request, err := admission.BuildRequest(fixture.store.RootID, strings.Repeat("c", 32), description.ID, fixture.workspace, requested, []byte("successor brief"), prior)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := task.MetaRecord{Predicate: reference, RequestedConfig: requested, Environment: []string{"HOME=/saved-home", "PATH=/saved/bin"}, EnvironmentRecorded: true}
	if native {
		// Native read-only records are admitted under the provider's plan mode.
		predecessor.EffectiveConfig.Approval = "plan"
		predecessor.EffectiveConfig.Policy = &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}
	}
	admitted, err := NativeCatalog().SuccessorCandidate(request, predecessor, scope)
	if err != nil {
		return false
	}
	definition, digest, err := admitted.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	requireNoSelector(t, description.ID+" binding environment", definition.Environment, []string{"/saved-home", "/saved/bin"})
	worker, err := scopedDependencies(Dependencies{ResourceChecker: fixtureResources, Environment: []string{"HOME=/ambient-home", "PATH=/ambient/bin"}}.normalized(), definition.Environment, true)
	if err != nil {
		t.Fatal(err)
	}
	recompiled, err := prepareInspectionWorkerCandidate(continuationDependencies(worker, predecessor), fixture.store.Root, request, nil)
	if err != nil {
		t.Fatalf("%s %s %s native=%t: worker could not recompile the bound successor: %v", description.ID, reference.Version, reference.SHA256[:8], native, err)
	}
	_, workerDigest, err := recompiled.Inspection.Snapshot()
	if err != nil || workerDigest != digest {
		t.Fatalf("%s %s %s native=%t: worker definition differs from the admitted binding (err=%v)", description.ID, reference.Version, reference.SHA256[:8], native, err)
	}
	return true
}
