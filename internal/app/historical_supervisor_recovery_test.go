package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type historicalSupervisorFixture struct {
	root              string
	store             *taskdir.Store
	td                *taskdir.TaskDir
	req               task.TaskRecord
	meta              task.MetaRecord
	submit            task.SubmitRecord
	receipt           task.SupervisorReceipt
	predecessor       task.SupervisorRef
	predecessorClient *pueue.Client
	successor         task.SupervisorRef
	current           *pueue.Client
	clientA           string
	daemonA           string
	clientB           string
	daemonB           string
	provider          string
	runner            string
	statusPath        string
	commandLog        string
	daemonStartLog    string
	readyPath         string
	options           pueue.Options
}

type historicalPrivateDaemonIdentity struct {
	Executable           string `json:"executable"`
	SHA256               string `json:"sha256"`
	ConfigPath           string `json:"config_path,omitempty"`
	ConfigDigest         string `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}

type historicalPrivateMigrationRecord struct {
	SchemaVersion            int                             `json:"schema_version"`
	Legacy                   historicalPrivateDaemonIdentity `json:"legacy"`
	Successor                historicalPrivateDaemonIdentity `json:"successor"`
	LegacyPID                int                             `json:"legacy_pid,omitempty"`
	LegacyProcessStart       string                          `json:"legacy_process_start,omitempty"`
	LegacyShutdownRequested  bool                            `json:"legacy_shutdown_requested,omitempty"`
	LegacyShutdownDispatched bool                            `json:"legacy_shutdown_dispatched,omitempty"`
	LegacyStopped            bool                            `json:"legacy_stopped,omitempty"`
}

type historicalPrivateMigrationHistory struct {
	SchemaVersion int                                `json:"schema_version"`
	Migrations    []historicalPrivateMigrationRecord `json:"migrations"`
}

type historicalPrivateBindingRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Binding       task.SupervisorRef `json:"binding"`
}

type historicalSupervisorOperation struct {
	name       string
	wantAction string
	run        func(*testing.T, *historicalSupervisorFixture) (int, []byte, []byte)
}

func TestHistoricalSupervisorRecoveryUsesCompletedSuccessor(t *testing.T) {
	for _, operation := range historicalSupervisorOperations() {
		for _, stopped := range []bool{false, true} {
			runHistoricalSupervisorScenario(t, operation, stopped)
		}
	}
}

func historicalSupervisorOperations() []historicalSupervisorOperation {
	return []historicalSupervisorOperation{
		{name: "status-reconciliation", run: runHistoricalStatusReconciliation},
		{name: "cancel-stop", wantAction: "kill", run: runHistoricalCancelStop},
		{name: "runner-reconciliation", run: runHistoricalRunnerReconciliation},
	}
}

func runHistoricalSupervisorScenario(t *testing.T, operation historicalSupervisorOperation, stopped bool) {
	t.Helper()
	t.Run(operation.name+"/"+historicalSupervisorStateName(stopped), func(t *testing.T) {
		fixture := newHistoricalSupervisorFixture(t, stopped)
		code, _, _ := operation.run(t, fixture)
		assertHistoricalSupervisorOperation(t, fixture, operation, code, stopped)
	})
}

func historicalSupervisorStateName(stopped bool) string {
	if stopped {
		return "stopped"
	}
	return "live"
}

func assertHistoricalSupervisorOperation(t *testing.T, fixture *historicalSupervisorFixture, operation historicalSupervisorOperation, code int, stopped bool) {
	t.Helper()
	if operation.wantAction == "" && code != 0 {
		t.Fatalf("historical %s returned exit code %d", operation.name, code)
	}
	assertHistoricalSupervisorControl(t, fixture, operation.wantAction, stopped)
	assertHistoricalTaskEvidence(t, fixture)
	assertHistoricalSupervisorState(t, fixture)
}

func runHistoricalStatusReconciliation(t *testing.T, fixture *historicalSupervisorFixture) (int, []byte, []byte) {
	code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{"status", fixture.req.TaskID, "--root", fixture.root, "--json"})
	if response.Admission != task.AdmissionAdmitted.String() || response.Liveness != task.LivenessRunning.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
		t.Fatalf("status lost the historical admission: %+v", response)
	}
	return code, stdout, stderr
}

func runHistoricalCancelStop(t *testing.T, fixture *historicalSupervisorFixture) (int, []byte, []byte) {
	code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{"cancel", fixture.req.TaskID, "--root", fixture.root, "--json"})
	if response.Stop == nil || !response.Stop.Matched || !response.Stop.Attempted || response.Stop.Action != "kill" || response.Stop.Acknowledged == nil || !*response.Stop.Acknowledged || response.Stop.NumericTaskID == nil || *response.Stop.NumericTaskID != 7 {
		t.Fatalf("cancel did not control the historical queue row: %+v", response)
	}
	return code, stdout, stderr
}

func runHistoricalSupervisorCommand(t *testing.T, fixture *historicalSupervisorFixture, args []string) (int, []byte, []byte, Response) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr, fixture.dependencies())
	stdoutBytes := append([]byte(nil), stdout.Bytes()...)
	stderrBytes := append([]byte(nil), stderr.Bytes()...)
	return code, stdoutBytes, stderrBytes, decodeHistoricalResponse(t, stdoutBytes, stderrBytes, code)
}

func runHistoricalRunnerReconciliation(t *testing.T, fixture *historicalSupervisorFixture) (int, []byte, []byte) {
	deps := fixture.dependencies()
	deps.PrepareProfile = func(request task.TaskRecord) (PreparedProfile, error) {
		return appTestExecutionProfile(&request, fixture.provider, nil), nil
	}
	deps.ExecutionHooks = execution.Hooks{Start: func(*exec.Cmd) error {
		return errors.New("historical supervisor provider start guard")
	}}
	var stdout, stderr bytes.Buffer
	code := RunRunner([]string{"--root", fixture.root, "--json", fixture.req.TaskID}, &stdout, &stderr, deps)
	stdoutBytes := append([]byte(nil), stdout.Bytes()...)
	stderrBytes := append([]byte(nil), stderr.Bytes()...)
	response := decodeHistoricalResponse(t, stdoutBytes, stderrBytes, code)
	if response.Admission != task.AdmissionAdmitted.String() || response.Liveness != task.LivenessRunning.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) || response.Outcome == nil || response.Publication != task.PublicationRejected.String() {
		t.Fatalf("runner did not reconcile the historical queue row before its provider boundary: exit=%d response=%+v", code, response)
	}
	return code, stdoutBytes, stderrBytes
}

func TestHistoricalSupervisorRecoveryRejectsUnrelatedBinding(t *testing.T) {
	fixture := newHistoricalSupervisorFixture(t, false)
	unrelated := fixture.predecessor
	unrelated.Endpoint = "unix:" + filepath.Join(fixture.root, ".supervisor", "run", "unrelated.sock")

	client, err := newSupervisorClient(t.Context(), fixture.root, unrelated, fixture.options, true, fixture.clientB)
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("unrelated historical supervisor binding was accepted")
	}
	if err == nil || (!errors.Is(err, pueue.ErrBinding) && !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired)) {
		t.Fatalf("unrelated historical supervisor binding error = %v, want a fail-closed binding refusal", err)
	}
	if matched, matchErr := pueue.PrivateSupervisorGenerationMatches(fixture.root, unrelated); matchErr != nil || matched {
		t.Fatalf("unrelated historical binding matched completed state: matched=%t err=%v", matched, matchErr)
	}
	assertHistoricalSupervisorState(t, fixture)
}

func TestCompletedPrivateMigrationRecoversPendingInspectionEdgeWithoutGenerationDemotion(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	recovery := prepareHistoricalInspectionRecovery(t, scenario)
	rebound := assertHistoricalInspectionRecoverySelectsSuccessor(t, scenario, recovery.deps)

	admitted, err := preparePrivateAdmissionSupervisor(recovery.deps, fixture.store, fixture.options)
	if err != nil {
		closeSupervisorMaintenance(admitted, err)
		t.Fatalf("fresh admission did not select completed successor B: %v", err)
	}
	defer closeSupervisorMaintenance(admitted, nil)
	if admitted.Binding() != fixture.successor {
		t.Fatalf("fresh admission selected %+v, want successor B %+v", admitted.Binding(), fixture.successor)
	}

	completeHistoricalInspectionRecovery(t, scenario, rebound, recovery.deps)
	assertHistoricalInspectionRecoveryState(t, scenario, recovery, admitted)
}

type historicalInspectionRecovery struct {
	historyPath    string
	historyBefore  []byte
	startsBefore   []byte
	groupAdds      *atomic.Int32
	observerErrors <-chan error
	deps           Dependencies
}

func prepareHistoricalInspectionRecovery(t *testing.T, scenario *historicalInspectionMigrationScenario) historicalInspectionRecovery {
	t.Helper()
	fixture := scenario.fixture
	legacyGroup := scenario.legacyGroup
	successorGroup := scenario.successorGroup
	// This is the crash point: private A→B is complete, while the exact group
	// edge is durable but CompleteGroupMigration has not run.
	err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		if prepareErr := inspection.PrepareGroupMigration(t.Context(), fixture.store, legacyGroup, successorGroup); prepareErr != nil {
			return prepareErr
		}
		if publishErr := pueue.PublishPrivateSupervisorBindingIfAbsent(fixture.root, fixture.predecessor); publishErr != nil {
			return publishErr
		}
		return fixture.current.MigratePrivateSupervisorWithLock(t.Context(), fixture.root)
	})
	if err != nil {
		t.Fatal(err)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.successor); completeErr != nil || !complete {
		t.Fatalf("fault fixture did not complete private A→B: complete=%t err=%v", complete, completeErr)
	}
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	if err = os.RemoveAll(scenario.legacyDir); err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateOrdinaryScheduling(fixture.root, fixture.successor, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatal(err)
	}

	historyPath := filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	historyBefore := readHistoricalSupervisorBytes(t, historyPath)
	startsBefore := readHistoricalSupervisorBytes(t, fixture.daemonStartLog)
	resetHistoricalSupervisorCommandLog(t, fixture)

	var groupAdds atomic.Int32
	observerErrors := make(chan error, 2)
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage != "completed" || !historicalCommandSequence(event.Argv, "group", "add") {
			return
		}
		groupAdds.Add(1)
		if updateErr := addHistoricalInspectionGroupStatus(fixture.statusPath, successorGroup.Name); updateErr != nil {
			select {
			case observerErrors <- updateErr:
			default:
			}
		}
	}
	return historicalInspectionRecovery{
		historyPath:    historyPath,
		historyBefore:  historyBefore,
		startsBefore:   startsBefore,
		groupAdds:      &groupAdds,
		observerErrors: observerErrors,
		deps:           fixture.dependencies(),
	}
}

func assertHistoricalInspectionRecoverySelectsSuccessor(t *testing.T, scenario *historicalInspectionMigrationScenario, deps Dependencies) task.SupervisorRef {
	t.Helper()
	fixture := scenario.fixture
	rebound, err := rebindContinuationSupervisor(t.Context(), fixture.root, fixture.predecessor, deps)
	if err != nil || rebound != fixture.successor {
		t.Fatalf("continuation rebind selected %+v, want successor B %+v: %v", rebound, fixture.successor, err)
	}
	status := supervisorCommand(Arguments{Root: fixture.root, SupervisorAction: "status"}, deps)
	if status.code != 0 || status.err != nil || status.response.SupervisorControl == nil || status.response.SupervisorControl.State != "migrated" {
		t.Fatalf("supervisor status did not select completed successor B: code=%d err=%v response=%+v", status.code, status.err, status.response.SupervisorControl)
	}
	return rebound
}

func completeHistoricalInspectionRecovery(t *testing.T, scenario *historicalInspectionMigrationScenario, rebound task.SupervisorRef, deps Dependencies) {
	t.Helper()
	fixture := scenario.fixture
	var revalidated task.SupervisorRef
	err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		var revalidateErr error
		revalidated, revalidateErr = revalidateContinuationSuccessor(t.Context(), fixture.root, rebound, deps.storeDependencies())
		return revalidateErr
	})
	if err != nil || revalidated != fixture.successor {
		t.Fatalf("continuation revalidation selected %+v, want successor B %+v: %v", revalidated, fixture.successor, err)
	}
	if complete, completeErr := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup); completeErr != nil || !complete {
		t.Fatalf("exclusive recovery did not complete exact A→B group edge: complete=%t err=%v", complete, completeErr)
	}
}

func assertHistoricalInspectionRecoveryState(t *testing.T, scenario *historicalInspectionMigrationScenario, recovery historicalInspectionRecovery, admitted *pueue.Client) {
	t.Helper()
	fixture := scenario.fixture
	if err := ensureInspectionGroup(fixture.store, admitted, inspection.AdmissionTimeout); err != nil {
		t.Fatalf("fresh admission did not establish successor inspection group: %v", err)
	}
	select {
	case observerErr := <-recovery.observerErrors:
		t.Fatal(observerErr)
	default:
	}
	if got := recovery.groupAdds.Load(); got != 1 {
		t.Fatalf("successor inspection group add count=%d, want exactly one", got)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || !migrated || head != scenario.successorGroup {
		t.Fatalf("recovered group head=%+v migrated=%t err=%v, want successor B", head, migrated, err)
	}
	assertHistoricalInspectionRecoveryArtifacts(t, scenario, recovery)
}

func assertHistoricalInspectionRecoveryArtifacts(t *testing.T, scenario *historicalInspectionMigrationScenario, recovery historicalInspectionRecovery) {
	t.Helper()
	fixture := scenario.fixture
	commands := string(readHistoricalSupervisorBytes(t, fixture.commandLog))
	if !strings.Contains(commands, "B status\n") || strings.Contains(commands, "A ") || strings.Contains(commands, " shutdown\n") {
		t.Fatalf("recovery contacted the wrong generation or shut one down: %q", commands)
	}
	if !bytes.Equal(recovery.startsBefore, readHistoricalSupervisorBytes(t, fixture.daemonStartLog)) {
		t.Fatal("recovery launched another supervisor daemon")
	}
	if !bytes.Equal(recovery.historyBefore, readHistoricalSupervisorBytes(t, recovery.historyPath)) {
		t.Fatal("recovery created a second private migration edge")
	}
	assertHistoricalMigrationJournalRetired(t, fixture)
	assertHistoricalActiveSupervisor(t, fixture)
}

func historicalCommandSequence(arguments []string, sequence ...string) bool {
	for index := 0; index+len(sequence) <= len(arguments); index++ {
		if slices.Equal(arguments[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func addHistoricalInspectionGroupStatus(path, groupName string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var status map[string]json.RawMessage
	if err = json.Unmarshal(data, &status); err != nil {
		return err
	}
	var groups map[string]json.RawMessage
	if err = json.Unmarshal(status["groups"], &groups); err != nil {
		return err
	}
	group, err := json.Marshal(map[string]any{"status": "Running", "parallel_tasks": 1})
	if err != nil {
		return err
	}
	groups[groupName] = group
	status["groups"], err = json.Marshal(groups)
	if err != nil {
		return err
	}
	data, err = json.Marshal(status)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func readHistoricalSupervisorBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHistoricalCancelRecoversBeforeMutationWhenSavedEndpointStopped(t *testing.T) {
	fixture := newHistoricalSupervisorFixture(t, true)
	pidPath := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.clientA, historicalSupervisorClientScript("A", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, fixture.daemonA, historicalSupervisorDaemonScript("A", fixture.readyPath, pidPath, fixture.daemonStartLog))
	resetHistoricalSupervisorCommandLog(t, fixture)

	code, _, _ := runHistoricalCancelStop(t, fixture)
	if code != 0 {
		t.Fatalf("cancel returned exit code %d", code)
	}
	commands, err := os.ReadFile(fixture.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(commands)
	if strings.Count(log, "A status\n") != 1 || strings.Contains(log, "A kill\n") || strings.Contains(log, "A remove\n") {
		t.Fatalf("cancel did not limit predecessor A to one readiness probe: %q", log)
	}
	if got := strings.Count(log, "B kill\n"); got != 1 {
		t.Fatalf("cancel sent successor mutation %d times, want exactly once: %q", got, log)
	}
	assertHistoricalTaskEvidence(t, fixture)
	assertHistoricalSupervisorState(t, fixture)
}

func newHistoricalSupervisorFixture(t *testing.T, stopped bool) *historicalSupervisorFixture {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, legacyDir := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	completeHistoricalSupervisorFixtureMigration(t, fixture, legacyDir, stopped)
	resetHistoricalSupervisorCommandLog(t, fixture)
	return fixture
}

func newHistoricalSupervisorFixturePaths(t *testing.T) *historicalSupervisorFixture {
	t.Helper()
	base := canonicalAppTestTempDir(t)
	return &historicalSupervisorFixture{
		root:           filepath.Join(base, "state"),
		statusPath:     filepath.Join(base, "status.json"),
		commandLog:     filepath.Join(base, "commands.log"),
		daemonStartLog: filepath.Join(base, "daemon-starts.log"),
		readyPath:      filepath.Join(base, "private-ready"),
		provider:       filepath.Join(base, "provider"),
		runner:         filepath.Join(base, "runner"),
	}
}

func prepareHistoricalSupervisorExecutables(t *testing.T, fixture *historicalSupervisorFixture) (string, string) {
	t.Helper()
	base := filepath.Dir(fixture.root)
	if err := os.WriteFile(fixture.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.daemonStartLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeRunnerFixture(t, fixture.provider, "#!/bin/sh\nexit 0\n")
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}

	legacyDir := filepath.Join(base, "legacy")
	successorDir := filepath.Join(base, "successor")
	pidPath := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	fixture.clientA = filepath.Join(legacyDir, "pueue")
	fixture.daemonA = filepath.Join(legacyDir, "pueued")
	fixture.clientB = filepath.Join(successorDir, "pueue")
	fixture.daemonB = filepath.Join(successorDir, "pueued")
	writeRunnerFixture(t, fixture.clientA, historicalSupervisorClientScript("A", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, fixture.daemonA, historicalSupervisorDaemonScript("A", fixture.readyPath, pidPath, fixture.daemonStartLog))
	writeRunnerFixture(t, fixture.clientB, historicalSupervisorClientScript("B", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, fixture.daemonB, historicalSupervisorDaemonScript("B", fixture.readyPath, pidPath, fixture.daemonStartLog))
	return workspace, legacyDir
}

func newHistoricalSupervisorOptions() pueue.Options {
	return pueue.Options{
		ObservationTimeout: pueue.DefaultObservationTimeout,
		Environment:        pueue.DefaultEnvironment(),
		SuccessfulCleanupAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error) {
			return pueue.PrivateQueueCleanupPlan{}, nil
		},
		PrivateMigrationAuthorizer: func(_ context.Context, _ string, _ task.SupervisorRef, jobs []pueue.Job) error {
			if len(jobs) != 0 {
				return fmt.Errorf("historical migration fixture observed %d queue jobs", len(jobs))
			}
			return nil
		},
	}
}

func prepareHistoricalSupervisorTask(t *testing.T, fixture *historicalSupervisorFixture, workspace string) {
	t.Helper()
	store, err := taskdir.InitStore(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	t.Cleanup(func() { fixture.close(t) })

	legacy, err := pueue.BindPrivate(t.Context(), fixture.clientA, fixture.daemonA, fixture.root, fixture.options)
	if err != nil {
		t.Fatalf("bind predecessor supervisor: %v", err)
	}
	fixture.current = legacy
	fixture.predecessorClient = legacy
	fixture.predecessor = legacy.Binding()

	brief := []byte("historical supervisor recovery")
	request := newHistoricalSupervisorTaskRecord(store.RootID, workspace, brief)
	profile := appTestExecutionProfile(&request, fixture.provider, nil)
	meta := newMeta(request, profile, fixture.predecessor, fixture.runner, task.RunnerOwnershipCustom, "historical-supervisor-test")
	fixture.td, err = store.CreateTask(request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	persistedRequest, persistedMeta, err := fixture.td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	fixture.req, fixture.meta = *persistedRequest, *persistedMeta
	recordHistoricalSupervisorAdmission(t, fixture)
}

func newHistoricalSupervisorTaskRecord(rootID, workspace string, brief []byte) task.TaskRecord {
	return task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		TaskID:        strings.Repeat("b", 32),
		Provider:      "fixture:test",
		Mode:          "read-only",
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief),
		BriefLength: int64(len(brief)),
	}
}

func recordHistoricalSupervisorAdmission(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	permit, err := fixture.td.PrepareSubmission(fixture.predecessor)
	if err != nil {
		t.Fatal(err)
	}
	fixture.submit, err = historicalConsumeSubmissionPermit(permit)
	if err != nil {
		t.Fatal(err)
	}
	numericTaskID := int64(7)
	observation := pueue.Observation{
		Matched: true,
		State:   pueue.StateRunning,
		Identity: pueue.Identity{
			RootID:        fixture.req.RootID,
			TaskID:        fixture.req.TaskID,
			SpecSHA256:    fixture.submit.SpecSHA256,
			MetaSHA256:    fixture.submit.MetaSHA256,
			NumericTaskID: &numericTaskID,
		},
		Binding:       fixture.predecessor,
		NumericTaskID: numericTaskID,
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	fixture.receipt = *receipt
}

func completeHistoricalSupervisorFixtureMigration(t *testing.T, fixture *historicalSupervisorFixture, legacyDir string, stopped bool) {
	t.Helper()
	successor, err := pueue.BindPrivate(t.Context(), fixture.clientB, fixture.daemonB, fixture.root, fixture.options)
	if err != nil {
		t.Fatalf("bind successor supervisor: %v", err)
	}
	fixture.current = successor
	fixture.successor = successor.Binding()
	if err = pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		return successor.MigratePrivateSupervisorWithLock(t.Context(), fixture.root)
	}); err != nil {
		t.Fatalf("complete explicit predecessor-to-successor migration: %v", err)
	}
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	if err = os.RemoveAll(legacyDir); err != nil {
		t.Fatal(err)
	}
	if stopped {
		stopHistoricalSupervisorFixture(t, fixture)
	}
}

func stopHistoricalSupervisorFixture(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), pueue.PrivateControlTimeout(fixture.options))
	if err := fixture.current.Shutdown(ctx); err != nil {
		cancel()
		t.Fatalf("stop successor fixture: %v", err)
	}
	if err := fixture.current.WaitUnavailable(ctx); err != nil {
		cancel()
		t.Fatalf("drain stopped successor fixture: %v", err)
	}
	cancel()
}

func resetHistoricalSupervisorCommandLog(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	if err := os.WriteFile(fixture.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func historicalConsumeSubmissionPermit(permit *taskdir.SubmissionPermit) (task.SubmitRecord, error) {
	record, err := permit.Record()
	if err != nil {
		return task.SubmitRecord{}, err
	}
	if err = permit.Consume(); err != nil {
		return task.SubmitRecord{}, err
	}
	if err = permit.Release(); err != nil {
		return task.SubmitRecord{}, err
	}
	return *record, nil
}

func (f *historicalSupervisorFixture) dependencies() Dependencies {
	return Dependencies{
		PredicateRegistry:           predicate.Default,
		SupervisorOptions:           f.options,
		InitialSupervisorExecutable: f.clientB,
		RunnerExecutable:            f.runner,
	}
}

func (f *historicalSupervisorFixture) close(t *testing.T) {
	t.Helper()
	if f.current != nil {
		if _, err := os.Stat(f.readyPath); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(f.options))
			if shutdownErr := f.current.Shutdown(ctx); shutdownErr != nil {
				t.Error(shutdownErr)
			} else if waitErr := f.current.WaitUnavailable(ctx); waitErr != nil {
				t.Error(waitErr)
			}
			cancel()
		}
		if err := f.current.Close(); err != nil {
			t.Error(err)
		}
	}
	if f.predecessorClient != nil {
		if err := f.predecessorClient.Close(); err != nil {
			t.Error(err)
		}
	}
	if f.td != nil {
		if err := f.td.Close(); err != nil {
			t.Error(err)
		}
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	}
}

func assertHistoricalSupervisorControl(t *testing.T, fixture *historicalSupervisorFixture, wantAction string, stopped bool) {
	t.Helper()
	data, err := os.ReadFile(fixture.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(data)
	if !strings.Contains(commands, "B status\n") {
		t.Fatalf("operation did not reach successor client B: %q", commands)
	}
	if strings.Contains(commands, "A ") {
		t.Fatalf("operation invoked removed predecessor client A: %q", commands)
	}
	if wantAction != "" {
		if got := strings.Count(commands, "B "+wantAction+"\n"); got != 1 {
			t.Fatalf("operation used successor action %q %d times, want exactly once: %q", wantAction, got, commands)
		}
	}
	starts, err := os.ReadFile(fixture.daemonStartLog)
	if err != nil {
		t.Fatal(err)
	}
	wantStarts := 1
	if stopped {
		wantStarts = 2
	}
	if got := strings.Count(string(starts), "B\n"); got < wantStarts {
		t.Fatalf("successor daemon starts=%d, want at least %d; starts=%q", got, wantStarts, starts)
	}
}

func assertHistoricalTaskEvidence(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	request, meta, err := fixture.td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	submit, err := fixture.td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.td.ReadSupervisorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.req, *request) || !reflect.DeepEqual(fixture.meta, *meta) || !reflect.DeepEqual(fixture.submit, *submit) || !reflect.DeepEqual(fixture.receipt, *receipt) {
		t.Fatal("historical task, meta, submit, or receipt record changed during recovery")
	}
	if meta.SupervisorConfig != fixture.predecessor || submit.Supervisor != fixture.predecessor || receipt.SupervisorRef() != fixture.predecessor {
		t.Fatalf("durable task evidence changed supervisor binding: meta=%+v submit=%+v receipt=%+v predecessor=%+v", meta.SupervisorConfig, submit.Supervisor, receipt.SupervisorRef(), fixture.predecessor)
	}
}

func assertHistoricalSupervisorState(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	assertHistoricalMigrationComplete(t, fixture)
	assertHistoricalSupervisorGenerations(t, fixture)
	assertHistoricalMigrationJournalRetired(t, fixture)
	assertHistoricalMigrationHistory(t, fixture)
	assertHistoricalActiveSupervisor(t, fixture)
	assertHistoricalActiveDaemon(t, fixture)
}

func assertHistoricalMigrationComplete(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.successor)
	if err != nil || !complete {
		t.Fatalf("successor generation is not complete: complete=%t err=%v", complete, err)
	}
	pending, err := pueue.PrivateSupervisorMigrationPending(fixture.root)
	if err != nil || pending {
		t.Fatalf("historical recovery left migration journal pending=%t err=%v", pending, err)
	}
}

func assertHistoricalSupervisorGenerations(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	for name, binding := range map[string]task.SupervisorRef{"predecessor": fixture.predecessor, "successor": fixture.successor} {
		matched, matchErr := pueue.PrivateSupervisorGenerationMatches(fixture.root, binding)
		if matchErr != nil || !matched {
			t.Fatalf("%s generation is not retained in completed state: matched=%t err=%v", name, matched, matchErr)
		}
	}
}

func assertHistoricalMigrationJournalRetired(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	migrationPath := filepath.Join(fixture.root, ".supervisor", "migration.json")
	if _, err := os.Stat(migrationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed historical operation created migration journal: %v", err)
	}
}

func assertHistoricalMigrationHistory(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	base := filepath.Join(fixture.root, ".supervisor")
	var history historicalPrivateMigrationHistory
	readHistoricalRecord(t, filepath.Join(base, "migration-history.json"), &history)
	if history.SchemaVersion != task.SchemaVersion || len(history.Migrations) != 1 {
		t.Fatalf("unexpected completed migration history: %+v", history)
	}
	migration := history.Migrations[0]
	assertHistoricalDaemonIdentity(t, "legacy migration history", migration.Legacy, fixture.predecessor)
	assertHistoricalDaemonIdentity(t, "successor migration history", migration.Successor, fixture.successor)
	if !migration.LegacyShutdownDispatched || !migration.LegacyStopped || migration.LegacyPID <= 0 {
		t.Fatalf("migration history did not record a stopped predecessor: %+v", migration)
	}
}

func assertHistoricalActiveSupervisor(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	base := filepath.Join(fixture.root, ".supervisor")
	var active historicalPrivateBindingRecord
	readHistoricalRecord(t, filepath.Join(base, "supervisor.identity.json"), &active)
	if active.SchemaVersion != task.SchemaVersion || active.Binding.ClientExecutable != fixture.successor.ClientExecutable || active.Binding.DaemonExecutable != fixture.successor.DaemonExecutable || !task.SameSupervisorIdentity(active.Binding, fixture.successor) {
		t.Fatalf("active supervisor identity is not successor B: %+v want=%+v", active.Binding, fixture.successor)
	}
}

func assertHistoricalActiveDaemon(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	base := filepath.Join(fixture.root, ".supervisor")
	var daemon historicalPrivateDaemonIdentity
	readHistoricalRecord(t, filepath.Join(base, "daemon.identity.json"), &daemon)
	if daemon.Executable != fixture.successor.DaemonExecutable || daemon.SHA256 != fixture.successor.DaemonSHA256 {
		t.Fatalf("active daemon provenance is not successor B: %+v want executable=%q sha=%q", daemon, fixture.successor.DaemonExecutable, fixture.successor.DaemonSHA256)
	}
}

func assertHistoricalDaemonIdentity(t *testing.T, label string, got historicalPrivateDaemonIdentity, want task.SupervisorRef) {
	t.Helper()
	wantIdentity := historicalPrivateDaemonIdentity{
		Executable:           want.DaemonExecutable,
		SHA256:               want.DaemonSHA256,
		ConfigPath:           want.ConfigPath,
		ConfigDigest:         want.ConfigDigest,
		ResolvedConfigSHA256: want.ResolvedConfigSHA256,
		Endpoint:             want.Endpoint,
	}
	if !reflect.DeepEqual(got, wantIdentity) {
		t.Fatalf("%s = %+v, want %+v", label, got, wantIdentity)
	}
}

func readHistoricalRecord(t *testing.T, path string, record any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err = task.DecodeStrict(data, record); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func decodeHistoricalResponse(t *testing.T, stdout, stderr []byte, code int) Response {
	t.Helper()
	var response Response
	if err := json.Unmarshal(stdout, &response); err != nil {
		t.Fatalf("decode app response exit=%d stdout=%q stderr=%q: %v", code, stdout, stderr, err)
	}
	return response
}

func historicalSupervisorResponseMatches(response *SupervisorResponse, wantID int64) bool {
	return response != nil && response.Matched && response.NumericTaskID != nil && *response.NumericTaskID == wantID
}

func historicalSupervisorClientScript(generation, readyPath, pidPath, statusPath, commandLog string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
command=
for argument in "$@"; do
  case "$argument" in
    --version) command=version ;;
    status|group|parallel|shutdown|kill|remove|add|enqueue) command="$argument" ;;
  esac
done
printf '%%s %%s\n' '%s' "$command" >> '%s'
case "$command" in
  version)
    printf 'pueue 4.0.4\n'
    ;;
  status)
    [ -f '%s' ] || exit 1
    if [ -f '%s' ]; then
      cat '%s'
    else
      printf '%%s\n' '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}'
    fi
    ;;
  group)
    [ -f '%s' ] || exit 1
    printf '%%s\n' '{"default":{"status":"Running","parallel_tasks":0}}'
    ;;
  parallel)
    [ -f '%s' ] || exit 1
    ;;
  add)
    printf '%%s\n' '--stashed --print-task-id'
    ;;
  enqueue)
    printf '%%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    ;;
  shutdown)
    pid="$(cat '%s' 2>/dev/null || true)"
    rm -f '%s'
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true
    fi
    ;;
  kill|remove)
    [ -f '%s' ] || exit 1
    ;;
  *)
    exit 64
    ;;
esac
`, generation, commandLog, readyPath, statusPath, statusPath, readyPath, readyPath, pidPath, readyPath, readyPath)
}

func historicalSupervisorDaemonScript(generation, readyPath, pidPath, daemonStartLog string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' '%s' >> '%s'
printf '%%s\n' "$$" > '%s'
touch '%s'
child=
cleanup() {
  rm -f '%s' '%s'
  if [ -n "$child" ]; then
    kill "$child" 2>/dev/null || true
  fi
  exit 0
}
trap cleanup TERM INT HUP
tail -f /dev/null &
child="$!"
wait "$child"
`, generation, daemonStartLog, pidPath, readyPath, readyPath, pidPath)
}
