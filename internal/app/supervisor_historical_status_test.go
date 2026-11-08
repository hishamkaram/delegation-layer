package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPublicSupervisorStatusReportsHistoricalReadiness(t *testing.T) {
	t.Parallel()
	for _, group := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			name := map[bool]string{false: "no inspection group", true: "inspection group"}[group] + "/" + map[bool]string{false: "installation present", true: "installation missing"}[missing]
			t.Run(name, func(t *testing.T) {
				runHistoricalStatusReadinessScenario(t, group, missing)
			})
		}
	}
}

func runHistoricalStatusReadinessScenario(t *testing.T, group, missing bool) {
	t.Helper()
	fixture := newHistoricalInstallationMigrationFixture(t, missing)
	if group {
		writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	}
	deps := fixture.dependencies()
	ready := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertReadyHistoricalSupervisorStatus(t, ready, "none")
	stopHistoricalInstallationPredecessor(t, fixture)
	configPath := pueue.PrivateConfigPath(fixture.root)
	configBefore := readSupervisorProbeTestFile(t, configPath)
	privateBefore := testsupport.CapturePairFaultWitness(t, filepath.Dir(configPath))
	startsBefore := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertStoppedHistoricalSupervisorStatus(t, result)
	testsupport.AssertPairFaultWitness(t, filepath.Dir(configPath), privateBefore)
	if !bytes.Equal(configBefore, readSupervisorProbeTestFile(t, configPath)) || !bytes.Equal(startsBefore, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("status changed historical config or started a daemon")
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("status changed historical task bytes")
	}
}

func assertReadyHistoricalSupervisorStatus(t *testing.T, result commandResult, wantAction string) {
	t.Helper()
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control == nil || control.NextAction != wantAction || control.LegacyReachability != "ready" || control.ReasonCode != "" {
		t.Fatalf("ready historical status changed: code=%d control=%+v err=%v", result.code, control, result.err)
	}
}

func assertStoppedHistoricalSupervisorStatus(t *testing.T, result commandResult) {
	t.Helper()
	if result.code != 1 || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped historical status lost its refusal: code=%d err=%v", result.code, result.err)
	}
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	control := decoded.SupervisorControl
	if control == nil || control.LegacyReachability != "unreachable" || control.ActiveWork != "unknown" || control.ReasonCode != "historical_target_not_ready" || control.NextAction != "stop_and_report" {
		t.Fatalf("stopped historical status invited a migrate loop: %+v", control)
	}
	if !strings.Contains(decoded.Error, "Automatic migration of a stopped historical generation is unavailable") || strings.Contains(decoded.Error, "supervisor migrate") {
		t.Fatalf("historical status omitted its limitation: %s", decoded.Error)
	}
}

func TestHistoricalSupervisorStatusDoesNotReclassifyObservationErrors(t *testing.T) {
	for _, cause := range []error{pueue.ErrBinding, pueue.ErrPrivateEndpointRefused, pueue.ErrStatusOperational, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			result := privateStatusUnavailable(newSupervisorCommandResponse("status"), false, true, cause)
			if result.code == 0 || result.err == nil || result.response.SupervisorControl.ReasonCode != "" || errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("operational observation became a clean historical refusal: %+v: %v", result.response.SupervisorControl, result.err)
			}
		})
	}
}

type historicalStatusMigrationPhase struct {
	name       string
	progress   historicalPrivateMigrationRecord
	wantAction string
	wantReason string
}

// These schema-valid journal phases are DATA fixtures for public guidance.
// They do not prove native shutdown, process identity or successful replay.
func TestHistoricalSupervisorStatusAndDispatchRespectMigrationPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []historicalStatusMigrationPhase{
		{name: "prepared", wantAction: "stop_and_report", wantReason: "historical_target_not_ready"},
		{name: "shutdown requested", progress: historicalPrivateMigrationRecord{LegacyPID: 101, LegacyProcessStart: "recorded-predecessor-start", LegacyShutdownRequested: true}, wantAction: "run_supervisor_migrate"},
		{name: "shutdown dispatched", progress: historicalPrivateMigrationRecord{LegacyPID: 101, LegacyProcessStart: "recorded-predecessor-start", LegacyShutdownRequested: true, LegacyShutdownDispatched: true}, wantAction: "run_supervisor_migrate"},
		{name: "legacy stopped", progress: historicalPrivateMigrationRecord{LegacyPID: 101, LegacyStopped: true}, wantAction: "run_supervisor_migrate"},
	} {
		for _, group := range []bool{false, true} {
			t.Run(phase.name+"/"+map[bool]string{false: "groupless", true: "group"}[group], func(t *testing.T) {
				runHistoricalStatusMigrationPhase(t, group, phase)
			})
		}
	}
}

func runHistoricalStatusMigrationPhase(t *testing.T, group bool, phase historicalStatusMigrationPhase) {
	t.Helper()
	fixture := newHistoricalInstallationMigrationFixture(t, false)
	if group {
		writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	}
	stopHistoricalInstallationPredecessor(t, fixture)
	writeHistoricalStatusMigrationPhase(t, fixture, phase.progress)
	before := testsupport.CapturePairFaultWitness(t, filepath.Dir(pueue.PrivateConfigPath(fixture.root)))
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	config := readSupervisorProbeTestFile(t, pueue.PrivateConfigPath(fixture.root))
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	result := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	control := result.response.SupervisorControl
	if result.code != 1 || control.NextAction != phase.wantAction || control.ReasonCode != phase.wantReason {
		t.Fatalf("journal phase lost status guidance: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	// Admission retains generic transition guidance until migration observes
	// the predecessor; status can report the stopped historical generation.
	assertHistoricalStatusPhaseDispatch(t, fixture)
	testsupport.AssertPairFaultWitness(t, filepath.Dir(pueue.PrivateConfigPath(fixture.root)), before)
	if !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("phase observation or admission refusal started a daemon")
	}
	if !bytes.Equal(config, readSupervisorProbeTestFile(t, pueue.PrivateConfigPath(fixture.root))) || !equalOutputUpgradeSnapshots(taskBefore, snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)) {
		t.Fatal("phase observation or admission refusal changed config or historical task bytes")
	}
}

func writeHistoricalStatusMigrationPhase(t *testing.T, fixture *historicalSupervisorFixture, record historicalPrivateMigrationRecord) {
	t.Helper()
	successor := fixture.predecessor
	successor.ClientExecutable, successor.DaemonExecutable = fixture.clientB, fixture.daemonB
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	record.SchemaVersion = task.SchemaVersion
	record.Legacy = historicalPrivateDaemonIdentityFor(fixture.predecessor)
	record.Successor = historicalPrivateDaemonIdentityFor(successor)
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return os.WriteFile(filepath.Join(fixture.root, ".supervisor", "migration.json"), data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if pending, readErr := pueue.PrivateSupervisorMigrationPending(fixture.root); readErr != nil || !pending {
		t.Fatalf("journal phase did not pass canonical decoding: pending=%t err=%v", pending, readErr)
	}
}

func assertHistoricalStatusPhaseDispatch(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	var client *pueue.Client
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		client, prepareErr = supervisor.PreparePrivateAdmissionTransition(ctx, fixture.dependencies().supervisorLookup(), fixture.store, fixture.options)
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, client, err)
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("recorded phase did not refuse admission with its canonical cause: %v", err)
	}
	response := newResponse("dispatch")
	response.TaskRecord = "not_created"
	var output bytes.Buffer
	if writeErr := writeJSONResponse(&output, "dispatch", &response, err); writeErr != nil {
		t.Fatal(writeErr)
	}
	failure := response.Failure
	if failure == nil || failure.Code != "supervisor_upgrade_required" || failure.NextAction != "run_supervisor_migrate" || failure.ReasonCode != "" || failure.RecoveryCommand == "" {
		t.Fatalf("canonical admission refusal lost public dispatch guidance: %+v: %s", failure, output.String())
	}
}

func TestHistoricalSupervisorStatusObservesDriftedSavedEndpoint(t *testing.T) {
	t.Parallel()
	fixture := newHistoricalInstallationMigrationFixture(t, false)
	configPath := pueue.PrivateConfigPath(fixture.root)
	original := prepareHistoricalStatusEndpointDrift(t, fixture)
	drifted := readSupervisorProbeTestFile(t, configPath)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	before := testsupport.CapturePairFaultWitness(t, filepath.Dir(configPath))
	commands := observePhaseStatusCommands(fixture)
	live := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	assertReadyHistoricalSupervisorStatus(t, live, "run_supervisor_migrate")
	assertHistoricalStatusTemporaryConfigsClosed(t, commands())
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	stopHistoricalInstallationPredecessor(t, fixture)
	if !componentDaemonsClosed(fixture.component.daemonWaits()) {
		t.Fatal("predecessor shutdown did not join the actual A Wait")
	}
	if err := os.WriteFile(configPath, drifted, 0o600); err != nil {
		t.Fatal(err)
	}
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	stopped := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	assertStoppedHistoricalSupervisorStatus(t, stopped)
	assertHistoricalStatusTemporaryConfigsClosed(t, commands())
	testsupport.AssertPairFaultWitness(t, filepath.Dir(configPath), before)
	if !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("status started a daemon for endpoint inspection")
	}
	if !bytes.Equal(drifted, readSupervisorProbeTestFile(t, configPath)) || !equalOutputUpgradeSnapshots(taskBefore, snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)) {
		t.Fatal("saved-endpoint observation changed config or historical task bytes")
	}
}

func prepareHistoricalStatusEndpointDrift(t *testing.T, fixture *historicalSupervisorFixture) []byte {
	t.Helper()
	configPath := pueue.PrivateConfigPath(fixture.root)
	original := readSupervisorProbeTestFile(t, configPath)
	waits := fixture.component.daemonWaits()
	t.Cleanup(func() {
		if !componentDaemonsClosed(waits) {
			if err := os.WriteFile(configPath, original, 0o600); err != nil {
				t.Error(err)
			}
		}
	})
	if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return os.WriteFile(configPath, retargetedPrivateSocketConfig(t, original, fixture.predecessor), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	// B refuses status on every endpoint. Preserve the
	// full authored protocol: the predecessor's cleanup also uses B transport.
	const statusOverride = `#!/bin/sh
set -eu
if [ "$#" -ge 3 ] && [ "$3" = status ]; then
  exit 1
fi
`
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.clientB, statusOverride+historicalSupervisorClientScript("B", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog))
	return original
}

func assertHistoricalStatusTemporaryConfigsClosed(t *testing.T, commands [][]string) {
	t.Helper()
	var found bool
	for _, command := range commands {
		if len(command) < 3 || command[1] != "-c" || !strings.HasPrefix(filepath.Base(command[2]), ".migration-pueue-") {
			continue
		}
		found = true
		if _, err := os.Stat(command[2]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("saved-endpoint observation retained a temporary config: %v", err)
		}
	}
	if !found {
		t.Fatal("endpoint drift did not exercise staged saved-endpoint config")
	}
}

func TestHistoricalSupervisorStatusPreservesUnreadableMigrationJournal(t *testing.T) {
	t.Parallel()
	for _, group := range []bool{false, true} {
		t.Run(map[bool]string{false: "groupless", true: "group"}[group], func(t *testing.T) {
			fixture := newHistoricalInstallationMigrationFixture(t, false)
			if group {
				writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
			}
			stopHistoricalInstallationPredecessor(t, fixture)
			path := filepath.Join(fixture.root, ".supervisor", "migration.json")
			if err := os.WriteFile(path, []byte("{]"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := testsupport.CapturePairFaultWitness(t, filepath.Dir(path))
			result := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
			var syntax *json.SyntaxError
			control := result.response.SupervisorControl
			if result.code != 1 || control.NextAction != "stop_and_report" || control.ReasonCode == "historical_target_not_ready" || !errors.As(result.err, &syntax) {
				t.Fatalf("migration read fault became an unavailable historical endpoint: control=%+v err=%v", control, result.err)
			}
			testsupport.AssertPairFaultWitness(t, filepath.Dir(path), before)
		})
	}
}

func TestSupervisorMigrationPhaseObservationPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, fixture := range []struct {
		name string
		ctx  context.Context
	}{{name: "nil context"}, {name: "canceled context", ctx: ctx}} {
		t.Run(fixture.name, func(t *testing.T) {
			resumePending, err := privateSupervisorMigrationResumePending(fixture.ctx, t.TempDir())
			if resumePending || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled journal observation invented a resume fact: pending=%t err=%v", resumePending, err)
			}
		})
	}
}
