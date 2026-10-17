package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestStatusDuringPreparedMigrationObservesExistingTask(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
	journalPath := filepath.Join(fixture.root, ".supervisor", "migration.json")
	journalBefore := readSupervisorProbeTestFile(t, journalPath)
	before := snapshotObservationPhaseRecords(t, fixture)
	commands := observePhaseStatusCommands(fixture)
	code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{
		"--root", fixture.root, "status", fixture.req.TaskID, "--json",
	})
	if code != 0 || response.Admission != task.AdmissionAdmitted.String() || response.Liveness != task.LivenessRunning.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
		t.Errorf("prepared migration blocked existing-task observation: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if response.Supervisor == nil || response.Supervisor.State != string(pueue.StateRunning) {
		t.Errorf("status did not report the existing Running row: %+v", response.Supervisor)
	}
	assertSupervisorProbeTestFile(t, journalPath, journalBefore)
	assertHistoricalTaskEvidence(t, fixture)
	assertObservationPhaseRecords(t, before)
	assertObservationPhaseNoMutation(t, commands())
}

func TestStatusDuringPreparedMigrationObservesQueuedAndReceiptedStashedTasks(t *testing.T) {
	for _, state := range []pueue.State{pueue.StateQueued, pueue.StateStashed} {
		t.Run(string(state), func(t *testing.T) {
			fixture := prepareHistoricalObservationPhase(t, state)
			prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
			before := snapshotObservationPhaseRecords(t, fixture)
			commands := observePhaseStatusCommands(fixture)
			code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{
				"--root", fixture.root, "status", fixture.req.TaskID, "--json",
			})
			if code != 0 || response.Admission != task.AdmissionAdmitted.String() || response.Liveness != task.LivenessRunning.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
				t.Errorf("prepared %s observation failed: code=%d stdout=%s stderr=%s", state, code, stdout, stderr)
			}
			if response.Supervisor == nil || response.Supervisor.State != string(state) {
				t.Errorf("status changed the observed %s row: %+v", state, response.Supervisor)
			}
			assertHistoricalTaskEvidence(t, fixture)
			assertObservationPhaseRecords(t, before)
			assertObservationPhaseNoMutation(t, commands())
		})
	}
}

func TestStatusDuringPreparedMigrationRejectsInvalidStashedReceipts(t *testing.T) {
	for _, test := range []struct {
		fault string
		code  int
	}{
		{fault: "missing", code: 1},
		{fault: "invalid", code: 1},
		{fault: "mismatched-meta", code: 1},
		{fault: "mismatched-row", code: 0},
	} {
		t.Run(test.fault, func(t *testing.T) {
			fixture := prepareHistoricalObservationPhase(t, pueue.StateStashed)
			prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
			faultObservationPhaseReceipt(t, fixture, test.fault)
			before := snapshotObservationPhaseRecords(t, fixture)
			commands := observePhaseStatusCommands(fixture)
			code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{
				"--root", fixture.root, "status", fixture.req.TaskID, "--json",
			})
			if code != test.code || response.Error == "" || response.Admission != task.AdmissionUnknown.String() {
				t.Errorf("%s Stashed receipt was accepted or misclassified: code=%d stdout=%s stderr=%s", test.fault, code, stdout, stderr)
			}
			if test.fault == "mismatched-row" && (response.Error != pueue.ErrUnknown.Error() || response.Supervisor == nil || response.Supervisor.Matched || response.Supervisor.NumericTaskID == nil || *response.Supervisor.NumericTaskID != 8) {
				t.Errorf("missing receipted row lost its explicit uncertainty: %+v", response)
			}
			assertObservationPhaseRecords(t, before)
			assertObservationPhaseNoMutation(t, commands())
		})
	}
}

func TestStatusRejectsUnsafeMigrationProgressWithoutMutation(t *testing.T) {
	for _, fault := range []string{"shutdown-requested", "foreign-predecessor"} {
		t.Run(fault, func(t *testing.T) {
			fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
			prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
			faultObservationPhaseJournal(t, fixture, fault)
			before := snapshotObservationPhaseRecords(t, fixture)
			commands := observePhaseStatusCommands(fixture)
			code, stdout, stderr, response := runHistoricalSupervisorCommand(t, fixture, []string{
				"--root", fixture.root, "status", fixture.req.TaskID, "--json",
			})
			if code == 0 || response.Error == "" || response.Admission == task.AdmissionAdmitted.String() {
				t.Errorf("unsafe %s journal granted observation: code=%d stdout=%s stderr=%s", fault, code, stdout, stderr)
			}
			assertHistoricalTaskEvidence(t, fixture)
			assertObservationPhaseRecords(t, before)
			assertObservationPhaseNoMutation(t, commands())
		})
	}
}

func TestPublicStatusRepairsCompleteCapabilitylessSchedulingReceipt(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	warm, err := supervisorcore.PreparePrivateAdmissionSupervisor(deps.InitialSupervisorExecutable, fixture.store, fixture.options)
	if err != nil || warm == nil {
		t.Fatalf("prepare complete scheduling fixture: client=%v err=%v", warm, err)
	}
	if closeErr := warm.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.predecessor); completeErr != nil || !complete {
		t.Fatalf("scheduling control is not complete: complete=%t err=%v", complete, completeErr)
	}
	path := privateOrdinarySchedulingTestPath(fixture.root)
	want := readSupervisorProbeTestFile(t, path)
	writeCapabilitylessActiveGenerationSchedulingReceipt(t, fixture.root)
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	resetHistoricalSupervisorCommandLog(t, fixture)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", fixture.root, "status", fixture.req.TaskID, "--json"}, &stdout, &stderr, deps)
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	if code != 0 || response.Admission != task.AdmissionAdmitted.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
		t.Errorf("public status did not repair healthy scheduling: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	assertSupervisorProbeTestFile(t, path, want)
	assertSupervisorProbeTestFile(t, fixture.daemonStartLog, starts)
	commands := readSupervisorProbeTestFile(t, fixture.commandLog)
	if count := bytes.Count(commands, []byte("A parallel\n")); count != 1 {
		t.Errorf("public status scheduling repair count=%d, want 1; commands=%q", count, commands)
	}
	assertHistoricalTaskEvidence(t, fixture)
}

func TestPublicStatusObservesCompletedHistoryThroughCompatiblePair(t *testing.T) {
	for _, test := range []struct {
		name         string
		removeDaemon bool
	}{
		{name: "client-and-daemon", removeDaemon: true},
		{name: "client-only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runObservationPhaseCompatiblePair(t, test.removeDaemon)
		})
	}
}

func runObservationPhaseCompatiblePair(t *testing.T, removeDaemon bool) {
	t.Helper()
	fixture, originalClient, originalDaemon := prepareObservationPhasePublishedStatePair(t)
	currentClient, currentDaemon := installObservationPhaseCompatiblePair(t, fixture, removeDaemon)
	if fixture.successor.ClientExecutable == originalClient || fixture.successor.DaemonExecutable == originalDaemon || currentClient == fixture.successor.ClientExecutable {
		t.Fatal("compatible recovery fixture did not retain separate published and available pairs")
	}
	if !removeDaemon && task.ComputeSHA256(readSupervisorProbeTestFile(t, currentDaemon)) != fixture.successor.DaemonSHA256 {
		t.Fatal("client-only compatible pair changed the retained daemon bytes")
	}
	missingPaths := []string{fixture.successor.ClientExecutable}
	if removeDaemon {
		missingPaths = append(missingPaths, fixture.successor.DaemonExecutable)
	}
	for _, path := range missingPaths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotObservationPhaseRecords(t, fixture)
	if !removeDaemon {
		path := fixture.successor.DaemonExecutable
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = observationPhaseRecord{data: readSupervisorProbeTestFile(t, path), info: info}
	}
	historyPath := filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	history := readSupervisorProbeTestFile(t, historyPath)
	commands := observePhaseStatusCommands(fixture)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = currentClient
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--root", fixture.root, "status", fixture.req.TaskID, "--json"}, &stdout, &stderr, deps)
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	assertObservationPhaseCompatibleResponse(t, code, stdout.Bytes(), stderr.Bytes(), response)
	for _, path := range missingPaths {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("compatible observation repaired a missing published executable: %s err=%v", path, statErr)
		}
	}
	if log := readSupervisorProbeTestFile(t, fixture.commandLog); !bytes.Contains(log, []byte("C status\n")) {
		t.Errorf("status did not use the current compatible pair: %q", log)
	}
	assertSupervisorProbeTestFile(t, historyPath, history)
	assertHistoricalTaskEvidence(t, fixture)
	assertObservationPhaseRecords(t, before)
	assertObservationPhaseNoMutation(t, commands())
}

func assertObservationPhaseCompatibleResponse(t *testing.T, code int, stdout, stderr []byte, response Response) {
	t.Helper()
	if code != 0 || response.Error != "" || response.Admission != task.AdmissionAdmitted.String() || response.Liveness != task.LivenessRunning.String() || !historicalSupervisorResponseMatches(response.Supervisor, 7) {
		t.Errorf("compatible pair could not observe completed historical task: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if response.Supervisor == nil || response.Supervisor.State != string(pueue.StateRunning) {
		t.Errorf("compatible observation did not retain the actual Running state: %+v", response.Supervisor)
	}
}

func prepareObservationPhasePublishedStatePair(t *testing.T) (*historicalSupervisorFixture, string, string) {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, legacyDir := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	originalClient, originalDaemon := fixture.clientB, fixture.daemonB
	var err error
	fixture.clientB, fixture.daemonB, err = supervisorcore.InstallStateSupervisorPairPaths(t.Context(), fixture.root, originalClient, originalDaemon)
	if err != nil {
		t.Fatal(err)
	}
	completeHistoricalSupervisorFixtureMigration(t, fixture, legacyDir, false)
	resetHistoricalSupervisorCommandLog(t, fixture)
	t.Cleanup(func() {
		// Restore only fixture-owned bytes after assertions so its existing
		// finalizer can close the still-live B daemon through its bound client.
		ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(fixture.options))
		defer cancel()
		if _, _, restoreErr := supervisorcore.InstallStateSupervisorPairPaths(ctx, fixture.root, originalClient, originalDaemon); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
	return fixture, originalClient, originalDaemon
}

func installObservationPhaseCompatiblePair(t *testing.T, fixture *historicalSupervisorFixture, replaceDaemon bool) (string, string) {
	t.Helper()
	base := filepath.Join(filepath.Dir(fixture.root), "compatible")
	client, daemon := filepath.Join(base, "pueue"), filepath.Join(base, "pueued")
	pidPath := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, client, historicalSupervisorClientScript("C", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	if replaceDaemon {
		writeRunnerFixture(t, daemon, historicalSupervisorDaemonScript("C", fixture.readyPath, pidPath, fixture.daemonStartLog))
	} else {
		daemon = fixture.successor.DaemonExecutable
	}
	installedClient, installedDaemon, err := supervisorcore.InstallStateSupervisorPairPaths(t.Context(), fixture.root, client, daemon)
	if err != nil {
		t.Fatal(err)
	}
	return installedClient, installedDaemon
}

func TestHeldSupervisorDuringPreparedMigrationRefusesParallelSetter(t *testing.T) {
	store, deps, legacy := newSupervisorProbeTestFixture(t)
	held, err := supervisorcore.PreparePrivateAdmissionSupervisor(deps.InitialSupervisorExecutable, store, deps.SupervisorOptions)
	if err != nil || held == nil {
		t.Fatalf("prepare complete held client: client=%v err=%v", held, err)
	}
	t.Cleanup(func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, held.Binding()); completeErr != nil || !complete {
		t.Fatalf("held client was not prepared from a complete phase: complete=%t err=%v", complete, completeErr)
	}
	prepareBusyAdmissionMigration(t, store.Root, legacy.Binding(), deps.SupervisorOptions)
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, held.Binding()); completeErr != nil || complete {
		t.Fatalf("migration did not invalidate the held client's complete phase: complete=%t err=%v", complete, completeErr)
	}
	base := filepath.Join(store.Root, ".supervisor")
	parallelPath := filepath.Join(base, "parallel")
	parallelBefore := readSupervisorProbeTestFile(t, parallelPath)
	journalPath := filepath.Join(base, "migration.json")
	journalBefore := readSupervisorProbeTestFile(t, journalPath)
	err = pueue.WithBootstrapLock(t.Context(), store.Root, func() error {
		_, setErr := held.SetPrivateDefaultParallelTasksWithBootstrapLock(t.Context(), store.Root, 0)
		return setErr
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("prepared held-client parallel setter error = %v, want migration required", err)
	}
	assertSupervisorProbeTestFile(t, parallelPath, parallelBefore)
	assertSupervisorProbeTestFile(t, journalPath, journalBefore)
}

func TestStashedStatusConsumerDefersWhenCompleteClientBecomesPrepared(t *testing.T) {
	for _, fault := range []string{"matching", "missing", "invalid", "mismatched-meta"} {
		t.Run(fault, func(t *testing.T) {
			runLateStashedObservationPhase(t, fault)
		})
	}
}

func TestReconcileStatusDoesNotReusePriorMaintenanceDeferral(t *testing.T) {
	prepared := prepareHistoricalObservationPhase(t, pueue.StateStashed)
	preparedClient := prepareCompleteStashedObservationClient(t, prepared)
	prepareBusyAdmissionMigration(t, prepared.root, prepared.predecessor, prepared.options)
	var observed tasklifecycle.Observation
	queue, err := tasklifecycle.ReconcileStatusWithClient(
		t.Context(), prepared.root, prepared.td, &prepared.req, &prepared.meta,
		prepared.submit.SpecSHA256, prepared.submit.MetaSHA256, preparedClient, &observed,
	)
	assertLateStashedReceiptError(t, "matching", err, nil)
	assertLateStashedObservationResult(t, "matching", queue, observed)
	if !observed.MaintenanceDeferred {
		t.Fatal("first prepared consumer did not establish the deferral reuse precondition")
	}

	healthy := prepareHistoricalObservationPhase(t, pueue.StateStashed)
	commands := observePhaseStatusCommands(healthy)
	prepareCompleteStashedObservationClient(t, healthy)
	before := snapshotObservationPhaseRecords(t, healthy)
	commandOffset := len(commands())
	queue, err = tasklifecycle.ReconcileStatus(
		t.Context(), healthy.root, healthy.td, &healthy.req, &healthy.meta,
		healthy.submit.SpecSHA256, healthy.submit.MetaSHA256,
		tasklifecycle.SupervisorOptions{Options: healthy.options, InitialExecutable: healthy.clientA, OpenStore: openNativeSupervisorStore}, &observed,
	)
	if err != nil || !queue.Matched || observed.Admission != task.AdmissionAdmitted {
		t.Errorf("healthy poll could not recover the receipted Stashed task: queue=%+v observed=%+v err=%v", queue, observed, err)
	}
	if observed.MaintenanceDeferred || observed.MaintenanceError != nil {
		t.Errorf("healthy poll retained another invocation's deferral: %+v", observed)
	}
	enqueues := 0
	for _, argv := range commands()[commandOffset:] {
		if slices.Contains(argv, "enqueue") && !slices.Contains(argv, "--help") {
			enqueues++
		}
	}
	if enqueues != 1 {
		t.Errorf("healthy poll attempted %d enqueues, want 1", enqueues)
	}
	assertHistoricalTaskEvidence(t, healthy)
	assertObservationPhaseRecords(t, before)
}

func runLateStashedObservationPhase(t *testing.T, fault string) {
	t.Helper()
	fixture := prepareHistoricalObservationPhase(t, pueue.StateStashed)
	commands := observePhaseStatusCommands(fixture)
	client := prepareCompleteStashedObservationClient(t, fixture)
	prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
	if fault != "matching" {
		faultObservationPhaseReceipt(t, fixture, fault)
	}
	before := snapshotObservationPhaseRecords(t, fixture)
	_, savedReceiptErr := fixture.td.ReadSupervisorReceipt()
	commandOffset := len(commands())
	var observed tasklifecycle.Observation
	queue, err := tasklifecycle.ReconcileStatusWithClient(
		t.Context(), fixture.root, fixture.td, &fixture.req, &fixture.meta,
		fixture.submit.SpecSHA256, fixture.submit.MetaSHA256, client, &observed,
	)
	assertLateStashedReceiptError(t, fault, err, savedReceiptErr)
	assertLateStashedObservationResult(t, fault, queue, observed)
	consumerCommands := commands()[commandOffset:]
	assertObservationPhaseNoMutation(t, consumerCommands)
	statuses := 0
	for _, argv := range consumerCommands {
		if slices.Contains(argv, "status") {
			statuses++
		}
	}
	wantStatuses := 0
	if fault == "matching" || fault == "missing" {
		wantStatuses = 2
	}
	if statuses != wantStatuses {
		t.Errorf("late %s receipt made %d status commands, want %d", fault, statuses, wantStatuses)
	}
	assertObservationPhaseRecords(t, before)
	if fault == "matching" {
		assertHistoricalTaskEvidence(t, fixture)
	}
}

func prepareCompleteStashedObservationClient(t *testing.T, fixture *historicalSupervisorFixture) *pueue.Client {
	t.Helper()
	prepared, err := supervisorcore.ObserveTaskSupervisor(
		t.Context(), fixture.store, fixture.predecessor, &fixture.meta,
		fixture.options, fixture.clientA, fixture.runner, openNativeSupervisorStore,
	)
	settleSupervisorProbeTestClient(t, prepared.Client, err)
	if err != nil || prepared.Client == nil || prepared.MaintenanceDeferred || prepared.MaintenanceError != nil {
		t.Fatalf("initial complete Stashed preparation deferred or failed: prepared=%+v err=%v", prepared, err)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, prepared.Client.RuntimeBinding()); completeErr != nil || !complete {
		t.Fatalf("initial Stashed client did not retain the Complete phase: complete=%t err=%v", complete, completeErr)
	}
	return prepared.Client
}

func assertLateStashedReceiptError(t *testing.T, fault string, err, savedReceiptErr error) {
	t.Helper()
	wantErr := error(nil)
	switch fault {
	case "matching":
	case "missing":
		wantErr = task.ErrEvidenceFault
	case "invalid":
		if err == nil || savedReceiptErr == nil || err.Error() != savedReceiptErr.Error() {
			t.Errorf("late malformed receipt lost its canonical decoder refusal: got=%v want=%v", err, savedReceiptErr)
		}
		return
	case "mismatched-meta":
		wantErr = task.ErrIdentityMismatch
	default:
		t.Fatalf("unknown late Stashed receipt case %q", fault)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("late %s receipt reconciliation error=%v, want %v", fault, err, wantErr)
	}
}

func assertLateStashedObservationResult(t *testing.T, fault string, queue pueue.Observation, observed tasklifecycle.Observation) {
	t.Helper()
	if fault == "matching" && observed.Admission != task.AdmissionAdmitted {
		t.Errorf("matching late Stashed receipt lost admission: %+v", observed)
	}
	if fault != "matching" && observed.Admission != task.AdmissionUnknown {
		t.Errorf("late %s receipt invented admission: %+v", fault, observed)
	}
	assertLateStashedDeferral(t, fault, queue, observed)
}

func assertLateStashedDeferral(t *testing.T, fault string, queue pueue.Observation, observed tasklifecycle.Observation) {
	t.Helper()
	if fault == "invalid" || fault == "mismatched-meta" {
		if observed.MaintenanceDeferred || observed.MaintenanceError != nil || queue.Matched {
			t.Errorf("invalid durable receipt reached late Stashed deferral: queue=%+v observed=%+v", queue, observed)
		}
		return
	}
	if !queue.Matched || queue.State != pueue.StateStashed || queue.NumericTaskID != 7 {
		t.Errorf("late Stashed deferral lost the actual row: %+v", queue)
	}
	if !observed.AdmissionObserved || !observed.MaintenanceDeferred || !supervisorcore.TaskMaintenanceCompletionRefused(observed.MaintenanceError) {
		t.Errorf("late Stashed deferral lost its typed pre-mutation refusal: %+v", observed)
	}
	if observed.Supervisor == nil || !observed.Supervisor.Matched || observed.Supervisor.State != string(pueue.StateStashed) {
		t.Errorf("late Stashed deferral invented a supervisor state: %+v", observed.Supervisor)
	}
}

func prepareHistoricalObservationPhase(t *testing.T, state pueue.State) *historicalSupervisorFixture {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeObservationPhaseStatus(t, fixture, state)
	return fixture
}

func writeObservationPhaseStatus(t *testing.T, fixture *historicalSupervisorFixture, state pueue.State) {
	t.Helper()
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	switch state {
	case pueue.StateRunning:
	case pueue.StateStashed:
		writeRunnerStashedStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	case pueue.StateQueued:
		writeObservationPhaseQueuedStatus(t, fixture.statusPath)
	case pueue.StateUnknown, pueue.StateEnded:
		t.Fatalf("unsupported observation fixture state %q", state)
	default:
		t.Fatalf("unsupported observation fixture state %q", state)
	}
	jobs, err := pueue.ParseStatus(readSupervisorProbeTestFile(t, fixture.statusPath), fixture.predecessor.ObservedVersion)
	if err != nil || len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].State != state {
		t.Fatalf("fixture did not publish actual %s status: jobs=%+v err=%v", state, jobs, err)
	}
}

func writeObservationPhaseQueuedStatus(t *testing.T, path string) {
	t.Helper()
	var status, tasks, row map[string]json.RawMessage
	data := readSupervisorProbeTestFile(t, path)
	if err := task.DecodeStrict(data, &status); err != nil {
		t.Fatal(err)
	}
	if err := task.DecodeStrict(status["tasks"], &tasks); err != nil {
		t.Fatal(err)
	}
	if err := task.DecodeStrict(tasks["7"], &row); err != nil {
		t.Fatal(err)
	}
	row["status"] = json.RawMessage(`{"Queued":{"enqueued_at":"2026-09-14T00:00:00.123456789Z"}}`)
	encoded, err := task.MarshalCanonical(row)
	if err != nil {
		t.Fatal(err)
	}
	tasks["7"] = encoded
	encoded, err = task.MarshalCanonical(tasks)
	if err != nil {
		t.Fatal(err)
	}
	status["tasks"] = encoded
	encoded, err = task.MarshalCanonical(status)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func faultObservationPhaseReceipt(t *testing.T, fixture *historicalSupervisorFixture, fault string) {
	t.Helper()
	path := filepath.Join(fixture.td.Dir, "supervisor.ref.json")
	var data []byte
	switch fault {
	case "missing":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	case "invalid":
		data = []byte("{}\n")
	case "mismatched-meta", "mismatched-row":
		receipt := fixture.receipt
		if fault == "mismatched-meta" {
			receipt.MetaSHA256 = task.ComputeSHA256([]byte("different admission metadata"))
		} else {
			receipt.NumericTaskID = 8
		}
		var err error
		data, err = task.MarshalCanonical(receipt)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown receipt fault %q", fault)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func faultObservationPhaseJournal(t *testing.T, fixture *historicalSupervisorFixture, fault string) {
	t.Helper()
	path := filepath.Join(fixture.root, ".supervisor", "migration.json")
	var record historicalPrivateMigrationRecord
	readHistoricalRecord(t, path, &record)
	switch fault {
	case "shutdown-requested":
		// A schema-valid pending shutdown cannot grant read-only observation.
		record.LegacyPID = 101
		record.LegacyProcessStart = "recorded-predecessor-start"
		record.LegacyShutdownRequested = true
	case "foreign-predecessor":
		record.Legacy.SHA256 = task.ComputeSHA256([]byte("unrelated predecessor"))
	default:
		t.Fatalf("unknown migration fault %q", fault)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type observationPhaseRecord struct {
	data []byte
	info os.FileInfo
}

func snapshotObservationPhaseRecords(t *testing.T, fixture *historicalSupervisorFixture) map[string]observationPhaseRecord {
	t.Helper()
	paths := []string{fixture.daemonStartLog}
	for _, name := range []string{"migration.json", "legacy-supervisor.json", "supervisor.identity.json", "daemon.identity.json", "ordinary-scheduling.json", "pueue-pair.json"} {
		paths = append(paths, filepath.Join(fixture.root, ".supervisor", name))
	}
	for _, name := range []string{"task.json", "meta.json", "submit.json", "supervisor.ref.json"} {
		paths = append(paths, filepath.Join(fixture.td.Dir, name))
	}
	before := make(map[string]observationPhaseRecord, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			before[path] = observationPhaseRecord{}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = observationPhaseRecord{data: data, info: info}
	}
	return before
}

func assertObservationPhaseRecords(t *testing.T, before map[string]observationPhaseRecord) {
	t.Helper()
	for path, want := range before {
		info, err := os.Stat(path)
		if want.info == nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("status created an absent record: path=%s err=%v", path, err)
			}
			continue
		}
		if err != nil {
			t.Error(err)
			continue
		}
		data := readSupervisorProbeTestFile(t, path)
		if !bytes.Equal(data, want.data) || info.Mode() != want.info.Mode() || !info.ModTime().Equal(want.info.ModTime()) || !os.SameFile(info, want.info) {
			t.Errorf("status changed durable record bytes, mode, time, or inode: %s", path)
		}
	}
}

func observePhaseStatusCommands(fixture *historicalSupervisorFixture) func() [][]string {
	var lock sync.Mutex
	var commands [][]string
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage != "entry" {
			return
		}
		lock.Lock()
		defer lock.Unlock()
		commands = append(commands, slices.Clone(event.Argv))
	}
	return func() [][]string {
		lock.Lock()
		defer lock.Unlock()
		return slices.Clone(commands)
	}
}

func assertObservationPhaseNoMutation(t *testing.T, commands [][]string) {
	t.Helper()
	for _, argv := range commands {
		for _, forbidden := range []string{"edit", "enqueue", "add", "parallel", "shutdown", "kill", "remove"} {
			if slices.Contains(argv, forbidden) && !slices.Contains(argv, "--help") {
				t.Errorf("observational status attempted supervisor mutation: %q", argv)
			}
		}
	}
}
