package pueue

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateMaintenanceProbeSkipsBusyLockAndCleansAfterRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		acquire func(context.Context, string, func() error) error
	}{
		{name: "shared", acquire: WithSupervisorLease},
		{name: "exclusive", acquire: WithBootstrapLock},
	} {
		t.Run(tc.name, func(t *testing.T) { testPrivateMaintenanceProbe(t, tc.acquire) })
	}
}

func TestPrivateMaintenanceCoalescesConcurrentTerminalTriggers(t *testing.T) {
	testPrivateTerminalTriggerCoalescing(t, "maintenance", func(client *Client) {
		client.MaintainPrivateQueue(context.Background())
	}, 1)
}

func TestScheduledPrivateMaintenanceCoalescesAcrossClients(t *testing.T) {
	scenario := newPrivateTerminalTriggerScenario(t, "cross-client-maintenance", true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	second, err := BindPrivate(ctx, scenario.fixture.clientPath, scenario.fixture.daemonPath,
		scenario.fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	cancel()
	supervisorLeaseTestSettleClient(t, second, err)
	if err != nil {
		t.Fatalf("bind second maintenance client: %v", err)
	}
	second.options = scenario.client.options

	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("first private maintenance was not scheduled")
	}
	waitForPrivateFixtureMarker(t, scenario.startedPath)
	if !second.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("second private maintenance was not coalesced")
	}

	secondClosed := make(chan error, 1)
	go func() { secondClosed <- second.Close() }()
	select {
	case closeErr := <-secondClosed:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("coalesced maintenance client waited behind the elected root owner")
	}
	if got := scenario.statusCommands.Load(); got != 1 {
		t.Fatalf("cross-client maintenance issued %d queue snapshots, want one", got)
	}

	scenario.releaseStatus(t)
	if closeErr := scenario.client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := scenario.statusCommands.Load(); got != 1 {
		t.Fatalf("elected cross-client maintenance issued %d queue snapshots, want one", got)
	}
	if got := scenario.removeCommands.Load(); got != 1 {
		t.Fatalf("elected cross-client maintenance issued %d removals, want one", got)
	}
	assertPrivateTerminalTriggerRemoved(t, scenario)
}

func TestScheduledPrivateMaintenanceRetainsTimedOutCommandUntilClose(t *testing.T) {
	scenario := newPrivateTerminalTriggerScenario(t, "scheduled-maintenance", true)
	scenario.client.options.ObservationTimeout = 50 * time.Millisecond
	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	waitForPrivateFixtureMarker(t, scenario.startedPath)

	closeDone := make(chan error, 1)
	go func() { closeDone <- scenario.client.Close() }()
	timeout := time.NewTimer(3 * scenario.client.options.ObservationTimeout)
	defer timeout.Stop()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("client close released timed-out maintenance before its child exited: %v", closeErr)
	case <-timeout.C:
	}

	scenario.releaseStatus(t)
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client close did not finish after the maintenance child exited")
	}
}

func TestScheduledPrivateMaintenanceWaitsForBusyGenerationLease(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	lease := supervisorLeaseTestHold(t, fixture.stateRoot, WithSupervisorLease)

	if !client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("scheduled maintenance skipped the busy generation lease: %v", closeErr)
	case <-time.After(250 * time.Millisecond):
	}

	lease.finish(t)
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled maintenance did not finish after the generation lease was released")
	}
	base := filepath.Dir(client.Binding().ConfigPath)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("scheduled maintenance removed unexpected numeric targets: %q", removed)
	}
}

func TestPrivateQueueMaintenanceTimeoutIsIndependentFromInspectionTimeout(t *testing.T) {
	wantDefault := time.Duration(privateQueueMaintenanceObservationPhases) * DefaultObservationTimeout
	if got := privateQueueMaintenanceTimeout(5 * time.Minute); got != wantDefault {
		t.Fatalf("maintenance timeout=%s, want phased cap %s", got, wantDefault)
	}
	wantShort := time.Duration(privateQueueMaintenanceObservationPhases) * 250 * time.Millisecond
	if got := privateQueueMaintenanceTimeout(250 * time.Millisecond); got != wantShort {
		t.Fatalf("short maintenance timeout=%s, want phased timeout %s", got, wantShort)
	}
}

type privateTerminalTriggerScenario struct {
	fixture        privateSupervisorTestFixture
	client         *Client
	base           string
	startedPath    string
	releasePath    string
	statusCommands atomic.Int64
	removeCommands atomic.Int64
	releaseOnce    sync.Once
}

func testPrivateTerminalTriggerCoalescing(t *testing.T, operation string, trigger func(*Client), wantRemove int64) {
	t.Helper()
	scenario := newPrivateTerminalTriggerScenario(t, operation, wantRemove != 0)
	runPrivateTerminalTriggerScenario(t, scenario, operation, trigger)
	if got := scenario.statusCommands.Load(); got != 1 {
		t.Fatalf("concurrent %s issued %d queue snapshots, want one", operation, got)
	}
	if got := scenario.removeCommands.Load(); got != wantRemove {
		t.Fatalf("concurrent %s issued %d removals, want %d", operation, got, wantRemove)
	}
	if wantRemove != 0 {
		assertPrivateTerminalTriggerRemoved(t, scenario)
	}
}

func newPrivateTerminalTriggerScenario(t *testing.T, operation string, recordKill bool) *privateTerminalTriggerScenario {
	t.Helper()
	fixture, client := supervisorLeaseTestBind(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	writeLargePrivateQueue(t, fixture.stateRoot, true)
	if recordKill {
		if err := os.WriteFile(filepath.Join(base, "test-record-kill"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scenario := &privateTerminalTriggerScenario{
		fixture:     fixture,
		client:      client,
		base:        base,
		startedPath: filepath.Join(base, operation+"-status-started"),
		releasePath: filepath.Join(base, operation+"-status-release"),
	}
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	client.options.Environment = append(os.Environ(), "PRIVATE_STATUS_STARTED="+scenario.startedPath, "PRIVATE_STATUS_RELEASE="+scenario.releasePath)
	client.options.Observer = scenario.observeCommand
	t.Cleanup(func() { scenario.releaseStatus(t) })
	return scenario
}

func privateTerminalTriggerCleanupAuthorizer(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) ([]int64, error) {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if job.State == StateEnded {
			ids = append(ids, job.ID)
		}
	}
	return ids, nil
}

func (scenario *privateTerminalTriggerScenario) observeCommand(event CommandEvent) {
	if event.Stage != "entry" {
		return
	}
	for _, argument := range event.Argv {
		switch argument {
		case "status":
			scenario.statusCommands.Add(1)
		case "remove":
			scenario.removeCommands.Add(1)
		}
	}
}

func (scenario *privateTerminalTriggerScenario) releaseStatus(t *testing.T) {
	scenario.releaseOnce.Do(func() {
		if err := os.WriteFile(scenario.releasePath, nil, 0o600); err != nil {
			t.Error(err)
		}
	})
}

func runPrivateTerminalTriggerScenario(t *testing.T, scenario *privateTerminalTriggerScenario, operation string, trigger func(*Client)) {
	t.Helper()
	leaderDone := make(chan struct{})
	go func() {
		trigger(scenario.client)
		close(leaderDone)
	}()
	waitForPrivateFixtureMarker(t, scenario.startedPath)

	callersReady, callersDone := startPrivateTerminalTriggerCallers(scenario.client, trigger)
	<-callersReady
	waitForPrivateTerminalTriggerCallers(t, scenario, operation, callersDone, leaderDone)
	scenario.releaseStatus(t)
	waitForPrivateTerminalTriggerLeader(t, operation, leaderDone)
}

func startPrivateTerminalTriggerCallers(client *Client, trigger func(*Client)) (<-chan struct{}, <-chan struct{}) {
	var callers sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(200)
	for range 200 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			ready.Done()
			trigger(client)
		}()
	}
	callersReady := make(chan struct{})
	go func() {
		ready.Wait()
		close(callersReady)
	}()
	callersDone := make(chan struct{})
	go func() {
		callers.Wait()
		close(callersDone)
	}()
	return callersReady, callersDone
}

func waitForPrivateTerminalTriggerCallers(t *testing.T, scenario *privateTerminalTriggerScenario, operation string, callersDone, leaderDone <-chan struct{}) {
	callerContext, cancelCallers := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCallers()
	select {
	case <-callersDone:
	case <-callerContext.Done():
		scenario.releaseStatus(t)
		<-leaderDone
		t.Fatalf("%s callers waited behind the elected trigger: %v", operation, callerContext.Err())
	}
}

func waitForPrivateTerminalTriggerLeader(t *testing.T, operation string, leaderDone <-chan struct{}) {
	leaderContext, cancelLeader := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelLeader()
	select {
	case <-leaderDone:
	case <-leaderContext.Done():
		t.Fatalf("elected %s did not complete: %v", operation, leaderContext.Err())
	}
}

func assertPrivateTerminalTriggerRemoved(t *testing.T, scenario *privateTerminalTriggerScenario) {
	removed := strings.Fields(string(readPrivateSupervisorFile(t, filepath.Join(scenario.base, "test-kill-task-id"))))
	if len(removed) != 199 {
		t.Fatalf("maintenance removed %d terminal rows, want 199", len(removed))
	}
}

func testPrivateMaintenanceProbe(t *testing.T, acquire func(context.Context, string, func() error) error) {
	t.Helper()
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Dir(client.Binding().ConfigPath)
	logOutput := captureMaintenanceProbeLog(t)
	var commands atomic.Int64
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" {
			commands.Add(1)
		}
	}
	authorizations := 0
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) ([]int64, error) {
		authorizations++
		assertMaintenanceProbeAuthorization(t, fixture.stateRoot, client.Binding(), ctx.Err(), root, binding, jobs)
		return []int64{0}, nil
	}
	lease := supervisorLeaseTestHold(t, fixture.stateRoot, acquire)

	// Callback entry in the holder proves contention before the fault call.
	// Returning before this deadline distinguishes skipping from waiting for
	// maintenance's observation timeout and suppressing that error.
	faultContext, cancelFault := context.WithTimeout(t.Context(), 250*time.Millisecond)
	client.MaintainPrivateQueue(faultContext)
	faultErr := faultContext.Err()
	cancelFault()
	if faultErr != nil {
		t.Fatalf("opportunistic maintenance waited for a busy lock: %v", faultErr)
	}
	if authorizations != 0 || commands.Load() != 0 {
		t.Fatalf("busy maintenance entered cleanup: authorizations=%d commands=%d", authorizations, commands.Load())
	}
	assertPrivateTestPathMissing(t, filepath.Join(base, "test-kill-task-id"))
	if output := logOutput(); output != "" {
		t.Fatalf("busy maintenance logged an error instead of silently skipping: %s", output)
	}
	// Required transitions still wait even though optional cleanup skips.
	supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
	lease.finish(t)

	cleanupContext, cancelCleanup := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelCleanup()
	client.MaintainPrivateQueue(cleanupContext)
	if authorizations != 1 || commands.Load() != 2 {
		t.Fatalf("available maintenance did not observe, authorize, and remove once: authorizations=%d commands=%d", authorizations, commands.Load())
	}
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("maintenance removed unexpected numeric targets: %q", removed)
	}
	if output := logOutput(); output != "" {
		t.Fatalf("successful maintenance logged an error: %s", output)
	}
}

func newMaintenanceProbeFixture(t *testing.T) (privateSupervisorTestFixture, *Client) {
	t.Helper()
	fixture, client := supervisorLeaseTestBind(t)
	base := filepath.Dir(client.Binding().ConfigPath)
	row := statusTestJob(0, "finished", statusStreamTestDoneState("Success"))
	data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "test-record-kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture, client
}

func assertMaintenanceProbeAuthorization(t *testing.T, wantRoot string, wantBinding task.SupervisorRef, ctxErr error, root string, binding task.SupervisorRef, jobs []Job) {
	t.Helper()
	if ctxErr != nil || root != wantRoot || binding != wantBinding {
		t.Errorf("cleanup authorization lost its context or supervisor binding: root=%q binding=%+v err=%v", root, binding, ctxErr)
	}
	if len(jobs) != 1 || jobs[0].ID != 0 || jobs[0].State != StateEnded || !jobs[0].Succeeded {
		t.Errorf("cleanup authorization did not receive the exact terminal snapshot: %+v", jobs)
	}
}

func captureMaintenanceProbeLog(t *testing.T) func() string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "maintenance-log-")
	if err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	log.SetOutput(file)
	t.Cleanup(func() {
		log.SetOutput(previous)
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return func() string {
		return string(readPrivateSupervisorFile(t, file.Name()))
	}
}
