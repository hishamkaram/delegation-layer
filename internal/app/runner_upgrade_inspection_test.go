package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestQueuedInspectionWorkerAcceptsAuthorizedUpgradeAfterOldBinaryRemoval(t *testing.T) {
	fixture := newQueuedInspectionUpgradeFixture(t)
	operation, err := inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.store.Root, operation, operation.Request().Binding, fixture.replacement.NewRunner); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unrecorded managed successor was accepted: %v", err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	if err = supervisorcore.AuthorizeQueuedInspectionRunnerUpgrade(context.Background(), fixture.store, fixture.replacement); err != nil {
		t.Fatalf("managed queue migration was not durably authorized: %v", err)
	}
	if err = os.Remove(fixture.oldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.store.Root, operation, operation.Request().Binding, fixture.replacement.NewRunner); err != nil {
		t.Fatalf("verified managed successor failed after old worker removal: %v", err)
	}
}

func TestAdmissionInspectionReplayAcceptsAuthorizedManagedSuccessor(t *testing.T) {
	fixture := newQueuedInspectionUpgradeFixture(t)
	operation, err := inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	original := operation.Request()
	if _, err = operation.ClaimSubmissionContext(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	workerDigest, err := commonprovider.FingerprintExecutable(fixture.replacement.NewRunner)
	if err != nil {
		t.Fatal(err)
	}
	binding := original.Binding
	binding.WorkerExecutable = fixture.replacement.NewRunner
	binding.WorkerSHA256 = workerDigest
	if _, err = admission.OpenAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unverified managed successor was accepted: %v", err)
	}

	if err = supervisorcore.AuthorizeQueuedInspectionRunnerUpgrade(context.Background(), fixture.store, fixture.replacement); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.oldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = admission.OpenAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now())
	if err != nil {
		t.Fatalf("authorized inspection admission replay failed: %v", err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	replayed := operation.Request()
	if replayed.Binding.WorkerExecutable != original.Binding.WorkerExecutable || replayed.Binding.WorkerSHA256 != original.Binding.WorkerSHA256 {
		t.Fatalf("replay changed immutable worker binding: before=%+v after=%+v", original.Binding, replayed.Binding)
	}
	if err = admission.SubmitInspectionOnce(context.Background(), operation, fixture.supervisor, binding.WorkerExecutable, fixture.store.Root); err != nil {
		t.Fatalf("already-submitted inspection was not reconciled through its original journal: %v", err)
	}
}

type completedManagedInspectionReplayFixture struct {
	store      *taskdir.Store
	request    task.TaskRecord
	supervisor *pueue.Client
	logPath    string
	binding    inspection.Binding
	oldRunner  string
	oldDigest  string
	newRunner  string
	newDigest  string
}

func newCompletedManagedInspectionReplayFixture(t *testing.T) completedManagedInspectionReplayFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, logPath := newBlockingInspectionSupervisor(t, store.RootID)

	oldSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, oldSource, "old inspection worker")
	oldRunner := installStateRunnerFixture(t, store.Root, oldSource)
	oldDigest, err := commonprovider.FingerprintExecutable(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: oldRunner, WorkerSHA256: oldDigest,
		RunnerOwnership: task.RunnerOwnershipManaged,
		Supervisor:      supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}

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
	if err = operation.RecordWorkerSuccess(17); err != nil {
		t.Fatal(err)
	}
	binding := operation.Request().Binding
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	newSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, newSource, "new inspection worker")
	newRunner := installStateRunnerFixture(t, store.Root, newSource)
	newDigest, err := commonprovider.FingerprintExecutable(newRunner)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	return completedManagedInspectionReplayFixture{
		store: store, request: request, supervisor: supervisor, logPath: logPath,
		binding: binding, oldRunner: oldRunner, oldDigest: oldDigest,
		newRunner: newRunner, newDigest: newDigest,
	}
}

func TestAdmissionInspectionReplayAcceptsCompletedManagedSuccessor(t *testing.T) {
	fixture := newCompletedManagedInspectionReplayFixture(t)
	binding := fixture.binding
	binding.WorkerExecutable = fixture.newRunner
	binding.WorkerSHA256 = fixture.newDigest
	replayed, err := admission.OpenAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now().UTC())
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
	if err = admission.ValidateInspectionSubmissionRunner(replayed, fixture.store, td, &fixture.request, meta, fixture.newRunner); err != nil {
		t.Fatalf("ordinary task replay rejected its completed managed inspection proof: %v", err)
	}

	before, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = admission.SubmitInspectionOnce(context.Background(), replayed, fixture.supervisor, fixture.newRunner, fixture.store.Root); err != nil {
		t.Fatalf("completed submitted operation was not replayable: %v", err)
	}
	after, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("replay launched or inspected another worker: before=%q after=%q", before, after)
	}
}

type queuedInspectionUpgradeFixture struct {
	store       *taskdir.Store
	supervisor  *pueue.Client
	request     task.TaskRecord
	replacement pueue.RunnerCommandReplacement
	oldRunner   string
}

func newQueuedInspectionUpgradeFixture(t *testing.T) queuedInspectionUpgradeFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, oldSource, "old inspection worker")
	oldRunner := installStateRunnerFixture(t, store.Root, oldSource)
	digest, err := commonprovider.FingerprintExecutable(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: oldRunner, WorkerSHA256: digest,
		RunnerOwnership: task.RunnerOwnershipManaged,
		Supervisor:      supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}
	label := identity.Label()
	job := pueue.Job{ID: 17, Label: &label, Group: identity.Group(), State: pueue.StateQueued}
	replacement, eligible, err := supervisorcore.PrepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("managed inspection worker was not eligible for queue migration")
	}
	newSource := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, newSource, "new inspection worker")
	replacement.NewRunner = installStateRunnerFixture(t, store.Root, newSource)
	return queuedInspectionUpgradeFixture{store: store, supervisor: supervisor, request: request, replacement: replacement, oldRunner: oldRunner}
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
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(&response, root, &task.TaskRecord{TaskID: strings.Repeat("d", 32), Provider: "pi:json"}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}, runner, commandRunner, nil, true, true)
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

	runnerDigest := task.ComputeSHA256([]byte("old managed runner"))
	if err = td.RecordManagedRunnerMigrationContext(context.Background(), runnerDigest); err != nil {
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
	requireLegacyRunnerRetry(t, root, store, td, meta)
}

func requireLegacyRunnerContinuation(t *testing.T, root string, req *task.TaskRecord) {
	t.Helper()
	loadedReq, loadedMeta, loadedBrief, err := loadContinuationInput(root, Arguments{TaskID: req.TaskID}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := buildContinuationArguments(Arguments{}, root, loadedReq, loadedMeta, loadedBrief)
	if err != nil {
		t.Fatal(err)
	}
	if continuation.Runner != "" || requestedRunnerOwnership(continuation) != task.RunnerOwnershipManaged {
		t.Fatalf("continuation retained removed legacy runner: %+v", continuation)
	}
}

func requireLegacyRunnerTimeoutContinuation(t *testing.T, root string, req *task.TaskRecord, meta *task.MetaRecord, runner, ownership string) {
	t.Helper()
	commandRunner := tasklifecycle.TimeoutContinuationCommandRunner(meta, ownership)
	response := newResponse("status")
	stopRecords := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(
		&response, root, &task.TaskRecord{TaskID: req.TaskID, Provider: "pi:json"}, stopRecords,
		NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"},
		runner, commandRunner, nil, true, true,
	)
	if response.Continuation == nil || !response.Continuation.Resumable || strings.Contains(response.Continuation.ContinueCommand, "--runner") {
		t.Fatalf("migrated legacy timeout continuation = %+v", response.Continuation)
	}
}

func requireLegacyRunnerRetry(t *testing.T, root string, store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) {
	t.Helper()
	currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, currentRunner, "current managed runner")
	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, root, store, td, meta)
	if err != nil || !allowUpgrade || refreshed.runnerOwnership != task.RunnerOwnershipManaged || !supervisorcore.IsStateRunnerPath(root, refreshed.Runner) {
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
	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, root, store, td, meta)
	if err != nil || !allowUpgrade || refreshed.runnerOwnership != task.RunnerOwnershipManaged || !supervisorcore.CurrentManagedRunnerExecutable(root, refreshed.Runner) {
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

func TestLegacyInspectionRetryKeepsOriginalRunner(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, false, "")
	oldRunner := filepath.Join(fixture.store.Root, ".supervisor", "delegate-run")
	writeRunnerFixture(t, oldRunner, "old managed inspection runner")
	meta := fixture.meta
	meta.RunnerExecutable = oldRunner
	td, err := fixture.store.CreateTask(fixture.req.TaskID, &fixture.req, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	currentRunner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, currentRunner, "current managed inspection runner")

	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, fixture.store.Root, fixture.store, td, &meta)
	if err != nil || allowUpgrade || refreshed.Runner != oldRunner || refreshed.runnerOwnership != "" {
		t.Fatalf("legacy inspection retry did not remain pinned: args=%+v allowUpgrade=%v err=%v", refreshed, allowUpgrade, err)
	}
	if recorded, recordErr := td.HasManagedRunnerMigration(); recordErr != nil || recorded {
		t.Fatalf("legacy inspection retry recorded ordinary runner ownership: recorded=%v err=%v", recorded, recordErr)
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

	got, err := admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{RunnerExecutable: managed}, custom)
	if err != nil {
		t.Fatal(err)
	}
	if got != managed {
		t.Fatalf("queue repair runner = %q, want managed runner %q", got, managed)
	}

	got, err = admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{RunnerExecutable: filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")}, custom)
	if err != nil || got != "" {
		t.Fatalf("missing managed queue repair runner = %q, err=%v, want skipped", got, err)
	}

	got, err = admissionQueueRepairRunner(Arguments{runnerOwnership: task.RunnerOwnershipManaged}, Dependencies{RunnerExecutable: custom}, managed)
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

	got, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.runnerOwnership != task.RunnerOwnershipManaged || !supervisorcore.IsStateRunnerPath(root, got.Runner) {
		t.Fatalf("managed retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if !supervisorcore.CurrentManagedRunnerExecutable(root, got.Runner) {
		t.Fatalf("refreshed state runner %q did not verify as current managed executable", got.Runner)
	}

	meta.SupervisorConfig.ConfigPath = filepath.Join(testsupport.CanonicalTempDir(t), "external-pueue.yml")
	got, allowUpgrade, err = refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.runnerOwnership != task.RunnerOwnershipManaged || !supervisorcore.IsStateRunnerPath(root, got.Runner) {
		t.Fatalf("external-supervisor retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if !supervisorcore.CurrentManagedRunnerExecutable(root, got.Runner) {
		t.Fatalf("external-supervisor retry runner %q did not verify as current managed executable", got.Runner)
	}

	custom := filepath.Join(testsupport.CanonicalTempDir(t), "custom-runner")
	writeRunnerFixture(t, custom, "custom runner")
	meta.RunnerExecutable = custom
	meta.RunnerOwnership = task.RunnerOwnershipCustom
	got, allowUpgrade, err = refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
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
	if err := validateContinuationMetadata(root, arguments, meta); err != nil {
		t.Fatal(err)
	}
	got, err := buildContinuationArguments(arguments, root, &task.TaskRecord{TaskID: "continuation"}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" || requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("managed continuation lost runner ownership: args=%+v", got)
	}
}
