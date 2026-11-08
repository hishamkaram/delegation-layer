package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestQueuedInspectionWorkerAcceptsAuthorizedUpgradeAfterOldBinaryRemoval(t *testing.T) {
	fixture := admissionfixture.NewQueuedInspectionUpgradeFixture(t)
	operation, err := inspection.LoadOperation(fixture.Store, fixture.Request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := supervisorcore.PlanQueuedRunnerRepairTarget(fixture.Replacement.NewRunner)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.Store.Root, operation, operation.Request().Binding, fixture.Replacement.NewRunner, target); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unrecorded managed successor was accepted: %v", err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	if err = supervisorcore.AuthorizeQueuedInspectionRunnerUpgrade(context.Background(), fixture.Store, fixture.Replacement, target); err != nil {
		t.Fatalf("managed queue migration was not durably authorized: %v", err)
	}
	if err = os.Remove(fixture.OldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = inspection.LoadOperation(fixture.Store, fixture.Request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.Store.Root, operation, operation.Request().Binding, fixture.Replacement.NewRunner, target); err != nil {
		t.Fatalf("verified managed successor failed after old worker removal: %v", err)
	}
}

type inspectionReplayFixture struct {
	store      *taskdir.Store
	request    task.TaskRecord
	supervisor *pueue.Client
	logPath    string
	binding    inspection.Binding
	oldRunner  string
	oldDigest  string
	newRunner  string
	newDigest  string
	candidate  commonprovider.ProfileCandidate
}

type inspectionReplayFixtureOptions struct {
	legacyWorker bool
	ownership    string
	missingProof bool
	failedProof  bool
}

func newCompletedManagedInspectionReplayFixture(t *testing.T) inspectionReplayFixture {
	t.Helper()
	fixture := newInspectionReplayFixture(t, inspectionReplayFixtureOptions{ownership: task.RunnerOwnershipManaged})
	if err := os.Remove(fixture.oldRunner); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func newInspectionReplayFixture(t *testing.T, options inspectionReplayFixtureOptions) inspectionReplayFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, logPath := newBlockingInspectionSupervisor(t, store.RootID)

	oldSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	if options.legacyWorker {
		oldSource = filepath.Join(store.Root, ".supervisor", "delegate-run")
	}
	writeRunnerFixture(t, oldSource, "old inspection worker")
	oldRunner := oldSource
	oldDigest := strings.Repeat("b", 64) // historical executable identity data
	helper := filepath.Join(testsupport.CanonicalTempDir(t), "inspection-helper")
	writeRunnerFixture(t, helper, "inspection helper")
	definition := commonprovider.InspectionDefinition{
		Revision: "fixture-inspection-v1", Executable: helper,
		ExecutableSHA256: strings.Repeat("a", 64), // historical executable identity data
		Arguments:        []string{"--fixed"}, Directory: request.CanonicalCwd,
		Environment: []string{"LANG=C"}, OutputLimit: 4096,
		Project: func([]byte) (json.RawMessage, error) {
			return nil, errors.New("completed replay unexpectedly ran the inspection projector")
		},
	}
	definition, definitionSHA, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA,
		HelperExecutable: helper, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: oldRunner, WorkerSHA256: oldDigest,
		RunnerOwnership: options.ownership,
		Environment:     definition.Environment,
		Supervisor:      supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	completeInspectionReplayFixture(t, operation, openedAt, options)
	binding := operation.Request().Binding
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	newSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, newSource, "new inspection worker")
	newRunner := newSource
	newDigest := strings.Repeat("c", 64) // historical executable identity data
	return inspectionReplayFixture{
		store: store, request: request, supervisor: supervisor, logPath: logPath,
		binding: binding, oldRunner: oldRunner, oldDigest: oldDigest,
		newRunner: newRunner, newDigest: newDigest,
		candidate: commonprovider.ProfileCandidate{Directory: request.CanonicalCwd, Inspection: &definition},
	}
}

func completeInspectionReplayFixture(t *testing.T, operation *inspection.Operation, openedAt time.Time, options inspectionReplayFixtureOptions) {
	t.Helper()
	submission, err := operation.ClaimSubmissionContext(context.Background(), openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	startedAt := openedAt.Add(2 * time.Millisecond)
	start, err := operation.ClaimStart(startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	completedAt := openedAt.Add(3 * time.Millisecond)
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), completedAt); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(17); err != nil {
		t.Fatal(err)
	}
	if !options.missingProof {
		if err = operation.RecordWorkerExitContext(t.Context(), 17, !options.failedProof); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmissionInspectionReplayAcceptsCompletedManagedSuccessor(t *testing.T) {
	fixture := newCompletedManagedInspectionReplayFixture(t)
	binding := fixture.binding
	binding.WorkerExecutable = fixture.newRunner
	binding.WorkerSHA256 = fixture.newDigest
	replayed, err := admission.OpenInspectionOperationContext(t.Context(), fixture.store, fixture.request, binding, time.Now().UTC())
	if err != nil {
		t.Fatalf("completed operation could not be reopened after worker upgrade: %v", err)
	}
	defer func() {
		if closeErr := replayed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if replayed.Request().Binding.WorkerExecutable != fixture.oldRunner || replayed.Request().Binding.WorkerSHA256 != fixture.oldDigest {
		t.Fatal("replay changed the immutable worker binding")
	}
	if authorized, authErr := replayed.ManagedWorkerUpgradeAuthorizedContext(context.Background()); authErr != nil || authorized {
		t.Fatalf("completed replay unexpectedly depended on queue authorization: authorized=%v err=%v", authorized, authErr)
	}
	meta := &task.MetaRecord{
		RunnerExecutable: fixture.newRunner,
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: fixture.binding.Supervisor,
	}
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.request.TaskID)}
	if err = admission.ValidateInspectionSubmissionRunner(replayed, fixture.store, td, &fixture.request, meta, fixture.newRunner, fixture.newRunner); err != nil {
		t.Fatalf("ordinary task replay rejected its completed managed inspection proof: %v", err)
	}

	before, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	facts, _, err := admission.RunInspection(replayed, fixture.supervisor, fixture.newRunner, fixture.store.Root)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("completed submitted operation facts=%s err=%v", facts, err)
	}
	after, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("replay launched or inspected another worker: before=%q after=%q", before, after)
	}
}

func TestPremetadataLegacyInspectionReplayUsesSelectedInstalledRunner(t *testing.T) {
	for _, unselected := range []bool{false, true} {
		name := "selected installation"
		if unselected {
			name = "explicit unselected runner"
		}
		t.Run(name, func(t *testing.T) {
			checkPremetadataLegacyInspectionReplaySelection(t, unselected)
		})
	}
}

func checkPremetadataLegacyInspectionReplaySelection(t *testing.T, unselected bool) {
	t.Helper()
	fixture := newInspectionReplayFixture(t, inspectionReplayFixtureOptions{legacyWorker: true})
	selection := supervisorcore.AdmissionSelection{}
	if unselected {
		selection.Runner = filepath.Join(testsupport.CanonicalTempDir(t), "unselected-runner")
		writeRunnerFixture(t, selection.Runner, "unselected inspection runner")
	}
	options := admission.InspectionOptions{
		Runner: selection.Runner, DefaultRunner: fixture.newRunner,
		RunnerOwnership: supervisorcore.RequestedRunnerOwnership(selection),
	}
	before := inspectionReplayRecords(t, fixture)
	logBefore, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, removeOld := range []bool{false, true} {
		if removeOld {
			if err = os.Remove(fixture.oldRunner); err != nil {
				t.Fatal(err)
			}
		}
		assertPremetadataLegacyInspectionReplayFacts(t, fixture, options, unselected)
		if after := inspectionReplayRecords(t, fixture); !maps.Equal(before, after) {
			t.Fatal("premetadata replay changed sealed inspection records")
		}
		assertAppInspectionNoOrdinaryTasks(t, fixture.store)
	}
	logAfter, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logBefore) != string(logAfter) {
		t.Fatal("completed legacy replay launched a supervisor command")
	}
}

func assertPremetadataLegacyInspectionReplayFacts(t *testing.T, fixture inspectionReplayFixture, options admission.InspectionOptions, unselected bool) {
	t.Helper()
	facts, _, replayErr := admission.InspectProviderContext(t.Context(), options, fixture.store, fixture.request, fixture.candidate, fixture.supervisor, true)
	if unselected {
		if !errors.Is(replayErr, task.ErrEvidenceFault) {
			t.Errorf("unselected runner error=%v, want evidence fault", replayErr)
		}
	} else if replayErr != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Errorf("premetadata completed legacy replay: facts=%s err=%v", facts, replayErr)
	}
}

func TestPremetadataLegacyInspectionReplayRequiresCompleteMatchingProof(t *testing.T) {
	for _, test := range []struct {
		name    string
		options inspectionReplayFixtureOptions
		change  func(*inspection.Binding, *task.TaskRecord)
	}{
		{name: "complete legacy proof", options: inspectionReplayFixtureOptions{legacyWorker: true}},
		{name: "custom binding", options: inspectionReplayFixtureOptions{legacyWorker: true, ownership: task.RunnerOwnershipCustom}},
		{name: "blank nonlegacy worker"},
		{name: "completion without exit proof", options: inspectionReplayFixtureOptions{legacyWorker: true, missingProof: true}},
		{name: "failed worker proof", options: inspectionReplayFixtureOptions{legacyWorker: true, failedProof: true}},
		{name: "helper drift", options: inspectionReplayFixtureOptions{legacyWorker: true}, change: func(binding *inspection.Binding, _ *task.TaskRecord) { binding.HelperExecutable += "-changed" }},
		{name: "definition drift", options: inspectionReplayFixtureOptions{legacyWorker: true}, change: func(binding *inspection.Binding, _ *task.TaskRecord) {
			binding.DefinitionSHA256 = strings.Repeat("d", 64)
		}},
		{name: "environment drift", options: inspectionReplayFixtureOptions{legacyWorker: true}, change: func(binding *inspection.Binding, _ *task.TaskRecord) { binding.Environment = []string{"LANG=changed"} }},
		{name: "supervisor drift", options: inspectionReplayFixtureOptions{legacyWorker: true}, change: func(binding *inspection.Binding, _ *task.TaskRecord) { binding.Supervisor.Endpoint += "-changed" }},
		{name: "logical request drift", options: inspectionReplayFixtureOptions{legacyWorker: true}, change: func(_ *inspection.Binding, request *task.TaskRecord) {
			request.BudgetNanos = int64(2 * time.Minute)
			request.RequestedConfig.Budget = "2m0s"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInspectionReplayFixture(t, test.options)
			before := inspectionReplayRecords(t, fixture)
			binding := fixture.binding
			// Open is downstream of installation selection; the higher consumer
			// test above covers truthful managed/custom selection at that boundary.
			runner, err := installation.ResolveRunner("", fixture.newRunner)
			if err != nil {
				t.Fatal(err)
			}
			binding.WorkerExecutable, binding.RunnerOwnership = runner, task.RunnerOwnershipManaged
			binding.WorkerExitProof = inspection.WorkerExitProofGuardianWaitV1
			request := fixture.request
			if test.change != nil {
				test.change(&binding, &request)
			}
			replayed, replayErr := admission.OpenInspectionOperationContext(t.Context(), fixture.store, request, binding, time.Now().UTC())
			assertInspectionReplayOpenResult(t, replayed, replayErr, test.name == "complete legacy proof")
			if after := inspectionReplayRecords(t, fixture); !maps.Equal(before, after) {
				t.Fatal("replay changed immutable inspection request or proof")
			}
			assertAppInspectionNoOrdinaryTasks(t, fixture.store)
		})
	}
}

func assertInspectionReplayOpenResult(t *testing.T, replayed *inspection.Operation, replayErr error, wantSuccess bool) {
	t.Helper()
	if replayed != nil {
		if err := replayed.Close(); err != nil {
			t.Error(err)
		}
	}
	if wantSuccess {
		if replayErr != nil {
			t.Errorf("completed legacy proof could not be reopened before task creation: %v", replayErr)
		}
	} else if !errors.Is(replayErr, task.ErrEvidenceFault) {
		t.Errorf("changed or unproven replay error=%v, want evidence fault", replayErr)
	}
}

func inspectionReplayRecords(t *testing.T, fixture inspectionReplayFixture) map[string]string {
	t.Helper()
	directory := filepath.Join(fixture.store.Root, "inspections", fixture.request.TaskID)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	records := make(map[string]string)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		records[entry.Name()] = string(data)
	}
	return records
}

func TestTimeoutContinuationDoesNotPinManagedRunner(t *testing.T) {
	root := testsupport.CanonicalTempDir(t)
	meta := &task.MetaRecord{
		RunnerExecutable: filepath.Join(root, ".supervisor", "delegate-run"+"-"+strings.Repeat("c", 64)),
		RunnerOwnership:  task.RunnerOwnershipManaged,
	}
	commandRunner := tasklifecycle.TimeoutContinuationCommandRunner(meta, supervisorcore.PersistedRunnerOwnership(root, meta))
	if commandRunner != "" {
		t.Fatalf("managed continuation pinned runner %q", commandRunner)
	}
	runner := meta.RunnerExecutable
	response := newResponse("status")
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyContinuationOffer(&response, root, tasklifecycle.TimeoutContinuation(&task.TaskRecord{TaskID: strings.Repeat("d", 32), Provider: "pi:json"}, records, NativeCatalog(), tasklifecycle.ContinuationSessionObserved, runner, commandRunner, nil, true, true))
	if response.Continuation == nil || !response.Continuation.Resumable || strings.Contains(response.Continuation.ContinueCommand, "--runner") {
		t.Fatalf("managed continuation command = %+v", response.Continuation)
	}
}

func TestLegacyManagedRunnerMigrationSurvivesRemovedInstallation(t *testing.T) {
	root := filepath.Join(testsupport.CanonicalTempDir(t), "state")
	oldRunner := filepath.Join(testsupport.CanonicalTempDir(t), "old-install", "delegate-run")
	writeRunnerFixture(t, oldRunner, "old managed runner")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	td, req, meta := newLegacyRunnerTask(t, store, oldRunner)
	defer closeAppTestTask(t, store, td)

	if err = td.RecordManagedRunnerMigrationContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	ownership, err := supervisorcore.RunnerOwnershipForTask(root, td, meta)
	if err != nil || ownership != task.RunnerOwnershipManaged {
		t.Fatalf("legacy runner ownership after installation removal = %q, err=%v", ownership, err)
	}
	requireLegacyRunnerContinuation(t, root, req)
	requireLegacyRunnerTimeoutContinuation(t, root, req, meta, oldRunner, ownership)
	requireLegacyRunnerRetry(t, root, td, meta)
}

func requireLegacyRunnerContinuation(t *testing.T, root string, req *task.TaskRecord) {
	t.Helper()
	loadedReq, loadedMeta, loadedBrief, err := tasklifecycle.LoadContinuationInput(root, dispatchRequest(Arguments{TaskID: req.TaskID}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), dispatchOptions(Dependencies{ResourceChecker: fixtureResources}, false))
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := tasklifecycle.BuildContinuationRequest(dispatchRequest(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), root, loadedReq, loadedMeta, loadedBrief)
	if err != nil {
		t.Fatal(err)
	}
	if continuation.Selection.Runner != "" || supervisorcore.RequestedRunnerOwnership(continuation.Selection) != task.RunnerOwnershipManaged {
		t.Fatalf("continuation retained removed legacy runner: %+v", continuation)
	}
}

func requireLegacyRunnerTimeoutContinuation(t *testing.T, root string, req *task.TaskRecord, meta *task.MetaRecord, runner, ownership string) {
	t.Helper()
	commandRunner := tasklifecycle.TimeoutContinuationCommandRunner(meta, ownership)
	response := newResponse("status")
	stopRecords := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyContinuationOffer(&response, root, tasklifecycle.TimeoutContinuation(&task.TaskRecord{TaskID: req.TaskID, Provider: "pi:json"}, stopRecords, NativeCatalog(), tasklifecycle.ContinuationSessionObserved, runner, commandRunner, nil, true, true))
	if response.Continuation == nil || !response.Continuation.Resumable || strings.Contains(response.Continuation.ContinueCommand, "--runner") {
		t.Fatalf("migrated legacy timeout continuation = %+v", response.Continuation)
	}
}

func requireLegacyRunnerRetry(t *testing.T, root string, td *taskdir.TaskDir, meta *task.MetaRecord) {
	t.Helper()
	currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, currentRunner, "current managed runner")
	refreshed, allowUpgrade, err := supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), currentRunner, root, td, meta)
	if err != nil || !allowUpgrade || refreshed.RunnerOwnership != task.RunnerOwnershipManaged || refreshed.Runner != currentRunner {
		t.Fatalf("migrated legacy retry runner = %+v, allowUpgrade=%v, err=%v", refreshed, allowUpgrade, err)
	}
}

func TestLegacyManagedRetryRecordsOwnershipForContinuation(t *testing.T) {
	root := filepath.Join(testsupport.CanonicalTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := filepath.Join(root, ".supervisor", "delegate-run")
	writeRunnerFixture(t, oldRunner, "old managed runner")
	td, req, meta := newLegacyRunnerTask(t, store, oldRunner)
	defer closeAppTestTask(t, store, td)

	currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, currentRunner, "current managed runner")
	refreshed, allowUpgrade, err := supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), currentRunner, root, td, meta)
	if err != nil || !allowUpgrade || refreshed.RunnerOwnership != task.RunnerOwnershipManaged || refreshed.Runner != currentRunner {
		t.Fatalf("legacy retry did not select the current managed runner: args=%+v allowUpgrade=%v err=%v", refreshed, allowUpgrade, err)
	}
	if recorded, recordErr := td.HasManagedRunnerMigration(); recordErr != nil || !recorded {
		t.Fatalf("retry did not persist managed ownership before runner replacement: recorded=%v err=%v", recorded, recordErr)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	ownership, err := supervisorcore.RunnerOwnershipForTask(root, td, meta)
	if err != nil || ownership != task.RunnerOwnershipManaged {
		t.Fatalf("retry ownership after old installation removal = %q err=%v", ownership, err)
	}
	requireLegacyRunnerContinuation(t, root, req)
}

func TestLegacyInspectionRetryAcceptsSelectedInstalledRunner(t *testing.T) {
	fixture, td := newPreparedLegacyInspectionRetry(t, legacyInspectionRetryCase{})
	currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, currentRunner, "current managed inspection runner")
	before := legacyInspectionRetryRecords(t, fixture, td)
	selection := refreshLegacyInspectionRetry(t, fixture, td, currentRunner)
	deps := legacyInspectionRetryDependencies(fixture, currentRunner)
	for _, removeOld := range []bool{false, true} {
		if removeOld {
			if err := os.Remove(fixture.workerPath); err != nil {
				t.Fatal(err)
			}
		}
		if err := admission.ValidateManagedRetrySubmissionRunnerContext(t.Context(), submissionValidationStore(deps), td, &fixture.req, &fixture.meta, selection.Runner, currentRunner); err != nil {
			t.Errorf("submission rejected selected installed runner (old removed=%t): %v", removeOld, err)
		}
		profile, err := prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, selection.Runner, selection.RunnerOwnership)
		if err != nil || profile.Plan.Directory != fixture.req.CanonicalCwd {
			t.Errorf("provider preparation rejected selected installed runner (old removed=%t): profile=%+v err=%v", removeOld, profile, err)
		}
		assertLegacyInspectionRetryRecords(t, fixture, td, before)
	}
	if fixture.meta.RunnerOwnership != "" || fixture.operation.Request().Binding.RunnerOwnership != "" {
		t.Fatal("retry changed immutable legacy ownership")
	}
}

type legacyInspectionRetryCase struct {
	taskOwnership    string
	bindingOwnership string
	nonLegacyWorker  bool
	missingProof     bool
	failedProof      bool
}

func newPreparedLegacyInspectionRetry(t *testing.T, test legacyInspectionRetryCase) (*appInspectionProofFixture, *taskdir.TaskDir) {
	t.Helper()
	store, request, workspace, base := newAppInspectionProofRequest(t)
	fixture := &appInspectionProofFixture{store: store, req: request}
	t.Cleanup(func() {
		closeAppInspectionFixture(t, fixture.operation, store)
	})
	workerPath := filepath.Join(store.Root, ".supervisor", "delegate-run")
	if test.nonLegacyWorker {
		workerPath = filepath.Join(base, "custom-worker")
	}
	if err := os.MkdirAll(filepath.Dir(workerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	workerPath, workerSHA, definition, definitionSHA := newAppInspectionProofDefinition(t, fixture, base, workspace, workerPath)
	fixture.workerPath = workerPath
	configureAppInspectionProofOperation(t, fixture, request, workspace, base, test.bindingOwnership, workerSHA, definition, definitionSHA)
	if !test.missingProof {
		recordAppInspectionProofResults(t, fixture, !test.failedProof)
		if test.failedProof {
			if err := fixture.operation.RecordWorkerExitContext(t.Context(), 7, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !test.missingProof && !test.failedProof {
		if _, err := fixture.operation.ReadProofContext(t.Context()); err != nil {
			t.Fatalf("fixture lacks successful immutable inspection proof: %v", err)
		}
	}
	fixture.meta.RunnerExecutable = filepath.Join(store.Root, ".supervisor", "delegate-run")
	if test.nonLegacyWorker {
		writeRunnerFixture(t, fixture.meta.RunnerExecutable, "old managed task runner")
	}
	fixture.meta.RunnerOwnership = test.taskOwnership
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &fixture.meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture, td
}

func refreshLegacyInspectionRetry(t *testing.T, fixture *appInspectionProofFixture, td *taskdir.TaskDir, currentRunner string) supervisorcore.AdmissionSelection {
	t.Helper()
	if recorded, err := td.HasManagedRunnerMigration(); err != nil || recorded {
		t.Fatalf("fixture must start without migration: recorded=%t err=%v", recorded, err)
	}
	selection, allowUpgrade, err := supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), currentRunner, fixture.store.Root, td, &fixture.meta)
	if err != nil || !allowUpgrade || selection.Runner != currentRunner || selection.RunnerOwnership != task.RunnerOwnershipManaged {
		t.Fatalf("legacy retry did not select installed runner: selection=%+v allowUpgrade=%t err=%v", selection, allowUpgrade, err)
	}
	if recorded, err := td.HasManagedRunnerMigration(); err != nil || !recorded {
		t.Fatalf("legacy retry did not persist migration: recorded=%t err=%v", recorded, err)
	}
	return selection
}

func legacyInspectionRetryDependencies(fixture *appInspectionProofFixture, currentRunner string) Dependencies {
	return Dependencies{
		ResourceChecker:  fixtureResources,
		RunnerExecutable: currentRunner,
		Environment:      os.Environ(),
		PrepareCandidate: func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
			return fixture.candidate, nil
		},
	}
}

func legacyInspectionRetryRecords(t *testing.T, fixture *appInspectionProofFixture, td *taskdir.TaskDir) map[string]string {
	t.Helper()
	paths := []string{filepath.Join(td.Dir, "task.json"), filepath.Join(td.Dir, "meta.json")}
	inspectionDir := filepath.Join(fixture.store.Root, "inspections", fixture.req.TaskID)
	entries, err := os.ReadDir(inspectionDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			paths = append(paths, filepath.Join(inspectionDir, entry.Name()))
		}
	}
	records := make(map[string]string, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		records[path] = string(data)
	}
	return records
}

func assertLegacyInspectionRetryRecords(t *testing.T, fixture *appInspectionProofFixture, td *taskdir.TaskDir, before map[string]string) {
	t.Helper()
	if after := legacyInspectionRetryRecords(t, fixture, td); !maps.Equal(before, after) {
		t.Fatal("retry changed immutable task, metadata or inspection records")
	}
}

func TestLegacyInspectionRetryRefusesUnprovenOrCustomSuccessor(t *testing.T) {
	for _, test := range []struct {
		name string
		legacyInspectionRetryCase
		unselected     bool
		wantSubmission error
		wantProvider   error
	}{
		{name: "custom task", legacyInspectionRetryCase: legacyInspectionRetryCase{taskOwnership: task.RunnerOwnershipCustom}, wantSubmission: task.ErrIdentityMismatch, wantProvider: task.ErrEvidenceFault},
		{name: "custom binding", legacyInspectionRetryCase: legacyInspectionRetryCase{bindingOwnership: task.RunnerOwnershipCustom}, wantSubmission: task.ErrIdentityMismatch, wantProvider: task.ErrEvidenceFault},
		{name: "blank nonlegacy binding", legacyInspectionRetryCase: legacyInspectionRetryCase{nonLegacyWorker: true}, wantSubmission: task.ErrIdentityMismatch, wantProvider: task.ErrEvidenceFault},
		{name: "unselected runner", unselected: true, wantSubmission: task.ErrIdentityMismatch, wantProvider: task.ErrEvidenceFault},
		{name: "failed proof", legacyInspectionRetryCase: legacyInspectionRetryCase{failedProof: true}, wantSubmission: task.ErrEvidenceFault, wantProvider: task.ErrEvidenceFault},
		{name: "missing proof", legacyInspectionRetryCase: legacyInspectionRetryCase{missingProof: true}, wantSubmission: task.ErrIdentityMismatch, wantProvider: os.ErrNotExist},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, td := newPreparedLegacyInspectionRetry(t, test.legacyInspectionRetryCase)
			currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
			writeRunnerFixture(t, currentRunner, "selected installed runner")
			selectedRunner := currentRunner
			ownership := test.taskOwnership
			if test.taskOwnership == "" {
				selection := refreshLegacyInspectionRetry(t, fixture, td, currentRunner)
				ownership = selection.RunnerOwnership
			}
			if test.unselected {
				selectedRunner = filepath.Join(testsupport.CanonicalTempDir(t), "unselected-runner")
				writeRunnerFixture(t, selectedRunner, "unselected runner")
			}
			before := legacyInspectionRetryRecords(t, fixture, td)
			deps := legacyInspectionRetryDependencies(fixture, currentRunner)
			if err := admission.ValidateManagedRetrySubmissionRunnerContext(t.Context(), submissionValidationStore(deps), td, &fixture.req, &fixture.meta, selectedRunner, currentRunner); !errors.Is(err, test.wantSubmission) {
				t.Errorf("submission error=%v, want %v", err, test.wantSubmission)
			}
			if _, err := prepareMatchedProfileForRunner(deps, fixture.store.Root, fixture.req, fixture.meta, fixture.store, selectedRunner, ownership); !errors.Is(err, test.wantProvider) {
				t.Errorf("provider preparation error=%v, want %v", err, test.wantProvider)
			}
			if fixture.finalizeCalls != 0 || fixture.projectCalls != 0 {
				t.Fatalf("refused successor crossed provider callbacks: finalize=%d project=%d", fixture.finalizeCalls, fixture.projectCalls)
			}
			assertLegacyInspectionRetryRecords(t, fixture, td, before)
		})
	}
}

func newLegacyRunnerTask(t *testing.T, store *taskdir.Store, runner string) (*taskdir.TaskDir, *task.TaskRecord, *task.MetaRecord) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("legacy task")
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	digest := task.ComputeSHA256([]byte("runner migration test"))
	req := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: strings.Repeat("e", 32),
		Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: requested,
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: req.TaskID,
		RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: "/tmp/fixture-provider", ProviderVersion: "fixture-v1",
		RunnerExecutable: runner, Environment: []string{"HOME=" + testsupport.CanonicalTempDir(t)},
		PublisherBuild: "runner-migration-test", PublisherVersion: "runner-migration-test", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: task.SupervisorRef{
			ClientExecutable: "/tmp/pueue", ClientSHA256: digest, ResolvedConfigSHA256: digest,
			ConfigPath: pueue.PrivateConfigPath(store.Root), ConfigDigest: digest,
			Endpoint: "unix:" + filepath.Join(store.Root, ".supervisor", "run", "pueue.sock"), ObservedVersion: pueue.FixtureVersion,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	return td, req, meta
}

func TestAdmissionQueueRepairUsesManagedRunnerForCustomTask(t *testing.T) {
	custom := filepath.Join(testsupport.CanonicalTempDir(t), "custom-delegate-run")
	managed := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, custom, "custom runner")
	writeRunnerFixture(t, managed, "managed runner")

	got, err := admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{ResourceChecker: fixtureResources, RunnerExecutable: managed}, custom)
	if err != nil {
		t.Fatal(err)
	}
	if got != managed {
		t.Fatalf("queue repair runner = %q, want managed runner %q", got, managed)
	}

	got, err = admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{ResourceChecker: fixtureResources, RunnerExecutable: filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")}, custom)
	if err != nil || got != "" {
		t.Fatalf("missing managed queue repair runner = %q, err=%v, want skipped", got, err)
	}

	got, err = admissionQueueRepairRunner(Arguments{runnerOwnership: task.RunnerOwnershipManaged}, Dependencies{ResourceChecker: fixtureResources, RunnerExecutable: custom}, managed)
	if err != nil || got != managed {
		t.Fatalf("managed task queue repair runner = %q, err=%v, want selected task runner %q", got, err, managed)
	}
}

func TestRefreshManagedRetryRunnerUsesCurrentStateRunner(t *testing.T) {
	root := testsupport.CanonicalTempDir(t)
	source := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "current managed runner")
	meta := &task.MetaRecord{
		RunnerExecutable: filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run"),
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(root)},
	}

	got, allowUpgrade, err := supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), source, root, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.RunnerOwnership != task.RunnerOwnershipManaged || got.Runner != source {
		t.Fatalf("managed retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if got.Runner != source {
		t.Fatalf("refreshed state runner %q did not verify as current managed executable", got.Runner)
	}

	meta.SupervisorConfig.ConfigPath = filepath.Join(testsupport.CanonicalTempDir(t), "external-pueue.yml")
	got, allowUpgrade, err = supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), source, root, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.RunnerOwnership != task.RunnerOwnershipManaged || got.Runner != source {
		t.Fatalf("external-supervisor retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if got.Runner != source {
		t.Fatalf("external-supervisor retry runner %q did not verify as current managed executable", got.Runner)
	}

	custom := filepath.Join(testsupport.CanonicalTempDir(t), "custom-runner")
	writeRunnerFixture(t, custom, "custom runner")
	meta.RunnerExecutable = custom
	meta.RunnerOwnership = task.RunnerOwnershipCustom
	got, allowUpgrade, err = supervisorcore.RefreshManagedRetryRunnerContext(t.Context(), supervisorAdmissionSelection(Arguments{}, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), source, root, nil, meta)
	if err != nil || allowUpgrade || got.Runner != "" {
		t.Fatalf("custom retry was rewritten: args=%+v allowUpgrade=%v err=%v", got, allowUpgrade, err)
	}
}

func TestManagedContinuationKeepsRunnerOwnership(t *testing.T) {
	root := testsupport.CanonicalTempDir(t)
	managedRunner := filepath.Join(testsupport.CanonicalTempDir(t), "old-install", "delegate-run")
	writeRunnerFixture(t, managedRunner, "old managed runner")
	meta := &task.MetaRecord{
		Environment:      []string{"HOME=/task"},
		RunnerExecutable: managedRunner,
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: task.SupervisorRef{ConfigPath: filepath.Join(testsupport.CanonicalTempDir(t), "pueue.yml")},
	}
	arguments := Arguments{Runner: managedRunner}
	if err := tasklifecycle.ValidateContinuationMetadata(root, supervisorAdmissionSelection(arguments, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), meta); err != nil {
		t.Fatal(err)
	}
	got, err := tasklifecycle.BuildContinuationRequest(dispatchRequest(arguments, Dependencies{ResourceChecker: fixtureResources}.withCapturedEnvironment()), root, &task.TaskRecord{TaskID: "continuation"}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Selection.Runner != "" || supervisorcore.RequestedRunnerOwnership(got.Selection) != task.RunnerOwnershipManaged {
		t.Fatalf("managed continuation lost runner ownership: args=%+v", got)
	}
}

// A test's explicit installed-role selection is data, never native ownership.
func appRunnerRepairTarget(t *testing.T, installed string) supervisorcore.RunnerRepairTarget {
	t.Helper()
	target, err := supervisorcore.PlanQueuedRunnerRepairTarget(installed)
	if err != nil {
		t.Fatal(err)
	}
	return target
}
