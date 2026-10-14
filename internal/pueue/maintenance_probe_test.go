package pueue

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateMaintenanceProbeSkipsBusyTransitionAndCleansAfterRelease(t *testing.T) {
	testPrivateMaintenanceProbe(t, WithBootstrapLock)
}

func TestPrivateMaintenanceCoalescesConcurrentTerminalTriggers(t *testing.T) {
	testPrivateTerminalTriggerCoalescing(t, "maintenance", func(client *Client) {
		client.MaintainPrivateQueue(t.Context())
	}, 2, 1)
}

func TestPrivateMaintenanceRetriesApplicationSettlingPlan(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	var authorizations atomic.Int64
	client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
		if authorizations.Add(1) == 1 {
			return PrivateQueueCleanupPlan{Settling: true}, nil
		}
		return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
	}
	if !client.MaintainPrivateQueue(t.Context()) {
		t.Fatal("private maintenance was unavailable")
	}
	if got := authorizations.Load(); got != 2 {
		t.Fatalf("settling cleanup authorization calls=%d want=2", got)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("settled maintenance removed unexpected numeric targets: %q", removed)
	}
}

func TestPrivateMaintenanceReleasesQueueIdentityWhileSettling(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	// The synchronization channels, not wall time, prove that maintenance
	// releases queue identity while settling. Leave enough command budget for
	// race instrumentation on slower builders.
	client.options.ObservationTimeout = time.Second
	firstAuthorization := make(chan struct{})
	leaseAttempted := make(chan struct{})
	leaseEntered := make(chan struct{})
	leaseRelease := make(chan struct{})
	var authorizations atomic.Int64
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, _ string, _ task.SupervisorRef, _ []Job) (PrivateQueueCleanupPlan, error) {
		if authorizations.Add(1) == 1 {
			close(firstAuthorization)
			select {
			case <-leaseAttempted:
			case <-ctx.Done():
				return PrivateQueueCleanupPlan{}, ctx.Err()
			}
			return PrivateQueueCleanupPlan{Settling: true}, nil
		}
		select {
		case <-leaseEntered:
			return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
		default:
			return PrivateQueueCleanupPlan{Settling: true}, nil
		}
	}

	maintenanceDone := make(chan bool, 1)
	go func() { maintenanceDone <- client.MaintainPrivateQueue(t.Context()) }()
	<-firstAuthorization
	leaseDone := make(chan error, 1)
	go func() {
		close(leaseAttempted)
		leaseDone <- client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
			close(leaseEntered)
			<-leaseRelease
			return nil
		})
	}()

	select {
	case <-leaseEntered:
	case available := <-maintenanceDone:
		t.Fatalf("maintenance finished before settlement released queue identity: available=%t", available)
	case <-time.After(2 * time.Second):
		t.Fatal("queue identity lease did not enter during settlement")
	}
	close(leaseRelease)
	if err := <-leaseDone; err != nil {
		t.Fatal(err)
	}
	select {
	case available := <-maintenanceDone:
		if !available {
			t.Fatal("private maintenance became unavailable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not finish after settlement")
	}
	if got := authorizations.Load(); got != 2 {
		t.Fatalf("settling cleanup authorization calls=%d want=2", got)
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("settled maintenance removed unexpected numeric targets: %q", removed)
	}
}

func TestScheduledPrivateMaintenanceCoalescesRequestsAcrossClientsWithoutWaiting(t *testing.T) {
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
	transitionReady, err := TryWithPrivateTransition(t.Context(), scenario.fixture.stateRoot, func() error { return nil })
	if err != nil || !transitionReady {
		t.Fatalf("prepare transition election: acquired=%t err=%v", transitionReady, err)
	}

	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("first private maintenance was not scheduled")
	}
	waitForPrivateFixtureMarker(t, scenario.startedPath)
	transitionCalled := false
	acquired, err := TryWithPrivateTransition(t.Context(), scenario.fixture.stateRoot, func() error {
		transitionCalled = true
		return nil
	})
	if err != nil || !acquired || !transitionCalled {
		t.Fatalf("maintenance blocked required transition election: acquired=%t called=%t err=%v", acquired, transitionCalled, err)
	}
	if !second.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("second private maintenance was not coalesced")
	}

	secondClosed := make(chan error, 1)
	go func() { secondClosed <- second.Close() }()
	waitForNonOwnerMaintenanceClient(t, secondClosed, "non-owner maintenance contender")
	if got := scenario.statusCommands.Load(); got != 1 {
		t.Fatalf("cross-client maintenance issued %d queue snapshots before release, want one active owner", got)
	}

	scenario.releaseStatus(t)
	if closeErr := scenario.client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := scenario.statusCommands.Load(); got != 4 {
		t.Fatalf("two durable cross-client requests issued %d queue snapshots, want two authorization/revalidation pairs", got)
	}
	if got := scenario.removeCommands.Load(); got != 2 {
		t.Fatalf("two durable cross-client requests issued %d removals, want two fixture removals", got)
	}
}

func TestScheduledPrivateMaintenanceResumesTimedOutCommandAndDrainsWaitingRequest(t *testing.T) {
	scenario := newPrivateTerminalTriggerScenario(t, "scheduled-maintenance", true)
	scenario.client.options.ObservationTimeout = 50 * time.Millisecond
	bindContext, cancelBind := context.WithTimeout(t.Context(), 10*time.Second)
	second, err := BindPrivate(bindContext, scenario.fixture.clientPath, scenario.fixture.daemonPath,
		scenario.fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	cancelBind()
	supervisorLeaseTestSettleClient(t, second, err)
	if err != nil {
		t.Fatalf("bind waiting maintenance client: %v", err)
	}
	second.options = scenario.client.options

	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("first private maintenance was not scheduled")
	}
	waitForPrivateFixtureMarker(t, scenario.startedPath)
	if !second.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("waiting private maintenance was not scheduled")
	}

	firstClosed := make(chan error, 1)
	secondClosed := make(chan error, 1)
	go func() { firstClosed <- scenario.client.Close() }()
	go func() { secondClosed <- second.Close() }()
	waitForNonOwnerMaintenanceClient(t, secondClosed, "non-owner timed-out maintenance contender")
	select {
	case closeErr := <-firstClosed:
		t.Fatalf("owner close released timed-out maintenance before its child exited: %v", closeErr)
	default:
	}

	scenario.releaseStatus(t)
	waitForPrivateMaintenanceCommands(t, scenario, 4, 2)
	record := waitForPrivateMaintenanceCompletion(t, scenario, firstClosed)
	if got := scenario.statusCommands.Load(); got != 4 {
		t.Fatalf("timed-out owner and waiting request issued %d queue snapshots, want two authorization/revalidation pairs", got)
	}
	if got := scenario.removeCommands.Load(); got != 2 {
		t.Fatalf("timed-out owner and waiting request issued %d removals, want one per durable generation in the immutable fixture", got)
	}
	if record.Requested != 2 || record.Attempted != 2 || record.Completed != 2 {
		t.Fatalf("timed-out maintenance state=%+v, want both requests completed without another callback", record)
	}
}

func TestRequestedPrivateMaintenanceCoalescesBurstWithoutWaiting(t *testing.T) {
	const contenderCount = 200

	scenario := newPrivateTerminalTriggerScenario(t, "requested-maintenance-burst", true)
	ctx := t.Context()
	if generation, err := requestPrivateQueueMaintenance(ctx, scenario.fixture.stateRoot); err != nil || generation != 1 {
		t.Fatalf("initial maintenance generation=%d err=%v, want generation 1", generation, err)
	}

	ownerDone := make(chan bool, 1)
	go func() {
		ownerDone <- scenario.client.RunRequestedPrivateQueueMaintenance(ctx)
	}()
	waitForPrivateFixtureMarker(t, scenario.startedPath)

	results, contendersDone := startRequestedPrivateMaintenanceContenders(
		ctx,
		scenario.client,
		scenario.fixture.stateRoot,
		contenderCount,
	)
	waitForRequestedPrivateMaintenanceContenders(t, scenario, contendersDone, ownerDone)
	assertRequestedPrivateMaintenanceContenders(t, results, 2, contenderCount+1)
	if got := scenario.statusCommands.Load(); got != 1 {
		t.Fatalf("maintenance burst issued %d queue snapshots before release, want one active owner", got)
	}
	if got := scenario.removeCommands.Load(); got != 0 {
		t.Fatalf("maintenance burst issued %d removals before release, want none", got)
	}
	assertPrivateQueueMaintenanceTestState(t, scenario.base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     contenderCount + 1,
		FullRequested: contenderCount + 1,
	})

	scenario.releaseStatus(t)
	waitForRequestedPrivateMaintenanceOwner(t, ownerDone)
	if err := scenario.client.Close(); err != nil {
		t.Fatal(err)
	}
	if got := scenario.statusCommands.Load(); got != 4 {
		t.Fatalf("maintenance burst issued %d queue snapshots, want two coalesced authorization/revalidation pairs", got)
	}
	if got := scenario.removeCommands.Load(); got != 2 {
		t.Fatalf("maintenance burst issued %d removals, want one per coalesced pass", got)
	}
	assertPrivateQueueMaintenanceTestState(t, scenario.base, privateQueueMaintenanceRecord{
		SchemaVersion: task.SchemaVersion,
		Requested:     contenderCount + 1,
		Attempted:     contenderCount + 1,
		Completed:     contenderCount + 1,
		FullRequested: contenderCount + 1,
		FullCompleted: contenderCount + 1,
	})
}

func TestScheduledPrivateMaintenanceSharesStableGenerationLease(t *testing.T) {
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
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		lease.finish(t)
		t.Fatal("scheduled maintenance starved behind a stable generation lease")
	}
	lease.finish(t)
	base := filepath.Dir(client.Binding().ConfigPath)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("scheduled maintenance removed unexpected numeric targets: %q", removed)
	}
}

func TestScheduledPrivateMaintenanceWaitsForGenerationTransition(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	transition := supervisorLeaseTestHold(t, fixture.stateRoot, WithBootstrapLock)

	if !client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("scheduled maintenance crossed an active generation transition: %v", closeErr)
	case <-time.After(250 * time.Millisecond):
	}

	transition.finish(t)
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled maintenance did not finish after the generation transition")
	}
}

func TestScheduledPrivateMaintenanceWaitsForQueueIdentityLease(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	leaseEntered := make(chan struct{})
	leaseRelease := make(chan struct{})
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
			close(leaseEntered)
			<-leaseRelease
			return nil
		})
	}()
	<-leaseEntered

	if !client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("scheduled maintenance crossed an active queue identity lease: %v", closeErr)
	case <-time.After(250 * time.Millisecond):
	}

	close(leaseRelease)
	if err := <-leaseDone; err != nil {
		t.Fatal(err)
	}
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled maintenance did not finish after queue identity publication")
	}
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if removed := string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))); removed != "0\n" {
		t.Fatalf("scheduled maintenance removed unexpected numeric targets: %q", removed)
	}
}

func TestScheduledPrivateMaintenanceAllowsQueueIdentityLeasesDuringAuthorization(t *testing.T) {
	scenario := newPrivateTerminalTriggerScenario(t, "identity-gate", true)
	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	waitForPrivateFixtureMarker(t, scenario.startedPath)

	leaseEntered := make(chan struct{})
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- scenario.client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
			close(leaseEntered)
			return nil
		})
	}()
	select {
	case <-leaseEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow maintenance authorization blocked a new queue identity lease")
	}
	if err := <-leaseDone; err != nil {
		t.Fatal(err)
	}

	scenario.releaseStatus(t)
	if closeErr := scenario.client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	assertPrivateTerminalTriggerRemoved(t, scenario)
}

func TestScheduledPrivateMaintenanceUsesRequestedScope(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		request func(context.Context, string) error
		wantIDs []int64
	}{
		{name: "inspection", request: RequestPrivateInspectionQueueMaintenance, wantIDs: []int64{0}},
		{name: "full", request: RequestPrivateQueueMaintenance, wantIDs: []int64{0, 2}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testPrivateMaintenanceRequestedScope(t, testCase.request, testCase.wantIDs)
		})
	}
}

func waitForNonOwnerMaintenanceClient(t *testing.T, closed <-chan error, name string) {
	t.Helper()
	select {
	case closeErr := <-closed:
		if closeErr != nil {
			t.Fatalf("%s close: %v", name, closeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s maintenance client did not finish after release", name)
	}
}

func waitForPrivateMaintenanceCompletion(t *testing.T, scenario *privateTerminalTriggerScenario, closed <-chan error) privateQueueMaintenanceRecord {
	t.Helper()
	watchdog := time.NewTimer(20 * time.Second)
	defer watchdog.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	closedCh := closed
	clientClosed := false
	var last privateQueueMaintenanceRecord
	for {
		record, err := readPrivateQueueMaintenanceRecord(scenario.base)
		if err != nil {
			t.Fatal(err)
		}
		last = record
		completed := record.Requested == 2 && record.Attempted == 2 && record.Completed == 2
		if clientClosed && completed {
			return record
		}
		select {
		case closeErr := <-closedCh:
			if closeErr != nil {
				t.Fatalf("timed-out maintenance owner close: %v", closeErr)
			}
			clientClosed = true
			closedCh = nil
		case <-poll.C:
		case <-watchdog.C:
			t.Fatalf("timed-out maintenance owner did not durably complete: client_closed=%t state=%+v", clientClosed, last)
		}
	}
}

func waitForPrivateMaintenanceCommands(t *testing.T, scenario *privateTerminalTriggerScenario, wantStatus, wantRemove int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		status := scenario.statusCommands.Load()
		remove := scenario.removeCommands.Load()
		if status >= wantStatus && remove >= wantRemove {
			return
		}
		select {
		case <-scenario.commandObserved:
		case <-ctx.Done():
			t.Fatalf("maintenance commands did not start: status=%d/%d remove=%d/%d: %v", status, wantStatus, remove, wantRemove, ctx.Err())
		}
	}
}

func testPrivateMaintenanceRequestedScope(t *testing.T, request func(context.Context, string) error, wantIDs []int64) {
	t.Helper()
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	rows := map[string]any{
		"0": statusTestJob(0, inspectionGroupPrefix+"observed", statusStreamTestDoneState("Success")),
		"1": statusTestJob(1, inspectionGroupPrefix+"waiting", statusStreamTestDoneState("Success")),
		"2": statusTestJob(2, "delegate:ordinary", statusStreamTestDoneState("Success")),
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 1} {
		if err := client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}

	var authorizedIDs []int64
	authorizationCalls := 0
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorizationCalls++
		authorizedIDs = authorizedIDs[:0]
		for _, job := range jobs {
			authorizedIDs = append(authorizedIDs, job.ID)
		}
		return privateTerminalTriggerCleanupAuthorizer(ctx, root, binding, jobs)
	}
	var lock sync.Mutex
	var statusArguments [][]string
	client.options.Observer = func(event CommandEvent) {
		recordPrivateMaintenanceStatus(&lock, &statusArguments, event)
	}
	if err := request(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.ScheduleRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	observed := append([][]string(nil), statusArguments...)
	lock.Unlock()
	removedIDs := strings.Fields(string(readPrivateSupervisorFile(t, filepath.Join(base, "test-kill-task-id"))))
	assertPrivateMaintenanceRequestedScope(t, observed, authorizedIDs, removedIDs, wantIDs, authorizationCalls)
}

func recordPrivateMaintenanceStatus(lock *sync.Mutex, statusArguments *[][]string, event CommandEvent) {
	if event.Stage != "entry" || !slices.Contains(event.Argv, "status") {
		return
	}
	lock.Lock()
	defer lock.Unlock()
	*statusArguments = append(*statusArguments, append([]string(nil), event.Argv...))
}

func assertPrivateMaintenanceRequestedScope(
	t *testing.T,
	statusArguments [][]string,
	authorizedIDs []int64,
	removedIDs []string,
	wantIDs []int64,
	authorizationCalls int,
) {
	t.Helper()
	if len(statusArguments) != 2 {
		t.Fatalf("status commands=%d, want authorization and revalidation", len(statusArguments))
	}
	for _, arguments := range statusArguments {
		if slices.Contains(arguments, "label%="+inspectionGroupPrefix) {
			t.Fatalf("event-backed maintenance used a label-filtered identity snapshot: %q", arguments)
		}
	}
	if authorizationCalls != 1 {
		t.Fatalf("cleanup authorization calls=%d, want 1", authorizationCalls)
	}
	slices.Sort(authorizedIDs)
	if !slices.Equal(authorizedIDs, wantIDs) {
		t.Fatalf("authorized maintenance candidates=%v, want %v", authorizedIDs, wantIDs)
	}
	wantRemoved := make([]string, 0, len(wantIDs))
	for _, id := range wantIDs {
		wantRemoved = append(wantRemoved, strconv.FormatInt(id, 10))
	}
	slices.Sort(removedIDs)
	if !slices.Equal(removedIDs, wantRemoved) {
		t.Fatalf("removed maintenance candidates=%v, want %v", removedIDs, wantRemoved)
	}
}

func TestScheduledPrivateMaintenanceTransfersPersistedWakeToProcessLauncher(t *testing.T) {
	t.Run("canceled caller", func(t *testing.T) {
		testScheduledPrivateMaintenanceTransfersPersistedWakeToProcessLauncher(t, false)
	})
	t.Run("expired caller", func(t *testing.T) {
		testScheduledPrivateMaintenanceTransfersPersistedWakeToProcessLauncher(t, true)
	})
}

func testScheduledPrivateMaintenanceTransfersPersistedWakeToProcessLauncher(t *testing.T, expired bool) {
	t.Helper()
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	client.options.ControlOwnerExecutable = filepath.Join(fixture.stateRoot, "delegate-run")
	type launch struct {
		executable string
		root       string
	}
	launched := make(chan launch, 1)
	client.options.PrivateMaintenanceLauncher = func(launchContext context.Context, executable, root string) error {
		if err := launchContext.Err(); err != nil {
			t.Fatalf("maintenance launcher received canceled context: %v", err)
		}
		launched <- launch{executable: executable, root: root}
		return nil
	}
	if err := RequestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	callerContext := privateMaintenanceDoneCallerContext(t, expired)
	if callerContext.Err() == nil {
		t.Fatal("maintenance caller context is unexpectedly live")
	}
	if !client.ScheduleRequestedPrivateQueueMaintenance(callerContext) {
		t.Fatal("private maintenance process was not scheduled")
	}
	select {
	case got := <-launched:
		if got.executable != client.options.ControlOwnerExecutable || got.root != fixture.stateRoot {
			t.Fatalf("maintenance launch=%+v", got)
		}
	default:
		t.Fatal("maintenance launcher was not called")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	pending, err := PrivateQueueMaintenanceRetryNeeded(fixture.stateRoot)
	if err != nil || !pending {
		t.Fatalf("transferred wake pending=%t err=%v", pending, err)
	}
}

func privateMaintenanceDoneCallerContext(t *testing.T, expired bool) context.Context {
	t.Helper()
	if expired {
		callerContext, cancelCaller := context.WithDeadline(t.Context(), time.Unix(0, 0))
		t.Cleanup(cancelCaller)
		return callerContext
	}
	callerContext, cancelCaller := context.WithCancel(t.Context())
	cancelCaller()
	return callerContext
}

func TestScheduledPrivateMaintenanceRetainsWakeWhenProcessLaunchFails(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	client.options.PrivateMaintenanceLauncher = func(context.Context, string, string) error {
		return errors.New("injected launch failure")
	}
	if err := RequestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if client.ScheduleRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("failed maintenance process launch was reported as scheduled")
	}
	pending, err := PrivateQueueMaintenanceRetryNeeded(fixture.stateRoot)
	if err != nil || !pending {
		t.Fatalf("failed launch wake pending=%t err=%v", pending, err)
	}
}

func TestAuthorizedTerminalRemovalClearsCallbackAfterSuccessfulRemove(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	if err := client.RegisterPrivateTerminalEvent(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}
	type observation struct {
		found bool
		err   error
	}
	observed := make(chan observation, 1)
	var once sync.Once
	client.options.Observer = func(event CommandEvent) {
		if event.Stage != "entry" || !strings.Contains(strings.Join(event.Argv, " "), "remove") {
			return
		}
		once.Do(func() {
			_, found, err := client.ReadPrivateTerminalEvent(t.Context(), 0)
			observed <- observation{found: found, err: err}
		})
	}
	snapshot := QueueSnapshot{Jobs: []Job{{ID: 0, State: StateEnded}}}
	if err := client.removeAuthorizedTerminalRows(t.Context(), snapshot, []int64{0}); err != nil {
		t.Fatal(err)
	}
	result := <-observed
	if result.err != nil || !result.found {
		t.Fatalf("terminal callback state at remove entry: found=%t err=%v", result.found, result.err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(t.Context(), 0); err != nil || found {
		t.Fatalf("terminal callback state after successful remove: found=%t err=%v", found, err)
	}
}

func TestInspectionMaintenanceAuthorizesOnlyRowsWithTerminalEvents(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	rows := map[string]any{
		"0": statusTestJob(0, inspectionGroupPrefix+"first", statusStreamTestDoneState("Success")),
		"1": statusTestJob(1, inspectionGroupPrefix+"second", statusStreamTestDoneState("Success")),
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 1} {
		if err := client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}

	var authorized []Job
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorized = append([]Job(nil), jobs...)
		return privateTerminalTriggerCleanupAuthorizer(ctx, fixture.stateRoot, client.Binding(), jobs)
	}
	snapshot, plan, err := client.authorizePrivateSuccessfulTasks(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 2 {
		t.Fatalf("cleanup snapshot jobs=%d, want 2 for identity revalidation", len(snapshot.Jobs))
	}
	if len(authorized) != 1 || authorized[0].ID != 0 {
		t.Fatalf("authorized candidates=%+v, want only event-backed row 0", authorized)
	}
	if !slices.Equal(plan.RemoveIDs, []int64{0}) {
		t.Fatalf("authorized removals=%v, want [0]", plan.RemoveIDs)
	}
}

func TestFullMaintenanceKeepsOrdinaryRowsAndRequiresInspectionEvents(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	rows := map[string]any{
		"0": statusTestJob(0, inspectionGroupPrefix+"observed", statusStreamTestDoneState("Success")),
		"1": statusTestJob(1, inspectionGroupPrefix+"delayed", statusStreamTestDoneState("Success")),
		"2": statusTestJob(2, "delegate:ordinary", statusStreamTestDoneState("Success")),
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 1} {
		if err := client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}

	var authorized []Job
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorized = append([]Job(nil), jobs...)
		return privateTerminalTriggerCleanupAuthorizer(ctx, fixture.stateRoot, client.Binding(), jobs)
	}
	snapshot, plan, err := client.authorizePrivateSuccessfulTasks(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 3 {
		t.Fatalf("cleanup snapshot jobs=%d, want 3 for identity revalidation", len(snapshot.Jobs))
	}
	authorizedIDs := make([]int64, 0, len(authorized))
	for _, job := range authorized {
		authorizedIDs = append(authorizedIDs, job.ID)
	}
	slices.Sort(authorizedIDs)
	if !slices.Equal(authorizedIDs, []int64{0, 2}) {
		t.Fatalf("authorized candidates=%v, want event-backed inspection 0 and ordinary 2", authorizedIDs)
	}
	slices.Sort(plan.RemoveIDs)
	if !slices.Equal(plan.RemoveIDs, []int64{0, 2}) {
		t.Fatalf("authorized removals=%v, want [0 2]", plan.RemoveIDs)
	}
}

func TestInspectionMaintenancePreservesDuplicateLabelEvidence(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	label := inspectionGroupPrefix + "duplicate"
	rows := map[string]any{
		"0": statusTestJob(0, label, statusStreamTestDoneState("Success")),
		"1": statusTestJob(1, label, statusStreamTestDoneState("Success")),
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 1} {
		if err := client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPrivateTerminalEvent(t.Context(), fixture.stateRoot, 0, "0"); err != nil {
		t.Fatal(err)
	}

	var authorized []Job
	client.options.SuccessfulCleanupAuthorizer = func(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorized = append([]Job(nil), jobs...)
		return PrivateQueueCleanupPlan{}, nil
	}
	if _, _, err := client.authorizePrivateSuccessfulTasks(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 2 || authorized[0].Label == nil || authorized[1].Label == nil || *authorized[0].Label != label || *authorized[1].Label != label {
		t.Fatalf("duplicate-label evidence was hidden from authorization: %+v", authorized)
	}
}

func TestInspectionMaintenanceWithoutConfiguredCallbackUsesExactReconciliation(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, base)
	callbackLine := "  callback: " + strconv.Quote(privateTerminalEventCallback(base)) + "\n"
	config := strings.Replace(privateConfigYAML(base), callbackLine, "", 1)
	if config == privateConfigYAML(base) {
		t.Fatal("test did not remove the private callback")
	}
	if err := os.WriteFile(PrivateConfigPath(fixture.stateRoot), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if enabled, err := client.PrivateTerminalEventsEnabled(t.Context()); err != nil || enabled {
		t.Fatalf("terminal callback enabled=%t err=%v, want disabled", enabled, err)
	}
	rows := map[string]any{
		"0": statusTestJob(0, inspectionGroupPrefix+"first", statusStreamTestDoneState("Success")),
		"1": statusTestJob(1, inspectionGroupPrefix+"second", statusStreamTestDoneState("Success")),
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var authorized []Job
	client.options.SuccessfulCleanupAuthorizer = func(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorized = append([]Job(nil), jobs...)
		return PrivateQueueCleanupPlan{}, nil
	}
	if _, _, err := client.authorizePrivateSuccessfulTasks(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if len(authorized) != 2 {
		t.Fatalf("callback-disabled reconciliation candidates=%d, want 2", len(authorized))
	}
}

func TestAuthorizedTerminalRevalidationRejectsNewDuplicateLabel(t *testing.T) {
	label := inspectionGroupPrefix + "same"
	before := QueueSnapshot{Jobs: []Job{{ID: 0, Label: &label, State: StateEnded, Group: "default", Succeeded: true}}}
	after := cloneQueueSnapshot(before)
	after.Jobs = append(after.Jobs, Job{ID: 1, Label: &label, State: StateEnded, Group: "default", Succeeded: true})
	if err := validateAuthorizedTerminalRows(before, after, []int64{0}); !errors.Is(err, ErrCleanupAuthorization) {
		t.Fatalf("new duplicate label revalidation error=%v, want cleanup authorization failure", err)
	}
}

func TestPrivateMaintenanceRejectsRowIdentityChangedAfterAuthorization(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	changedLabel := "delegate:changed:identity"
	client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
		row := statusTestJob(0, changedLabel, statusStreamTestDoneState("Success"))
		data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
		if err := os.WriteFile(filepath.Join(base, "test-status.json"), data, 0o600); err != nil {
			return PrivateQueueCleanupPlan{}, err
		}
		return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
	}
	logOutput := captureMaintenanceProbeLog(t)
	if !client.MaintainPrivateQueue(t.Context()) {
		t.Fatal("private maintenance was unavailable")
	}
	if _, err := os.Lstat(filepath.Join(base, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed numeric ID was removed: %v", err)
	}
	if output := logOutput(); !strings.Contains(output, ErrCleanupAuthorization.Error()) {
		t.Fatalf("changed identity cleanup log=%q, want authorization failure", output)
	}
}

func TestScheduledPrivateMaintenanceDefersIncompleteGeneration(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if !isPrivateClient(client) {
		t.Fatal("maintenance fixture did not create a private client")
	}
	if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !isPrivateClient(client) {
		t.Fatal("bootstrap journal changed private client identity")
	}
	logOutput := captureMaintenanceProbeLog(t)
	if _, err := requestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.ScheduleRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete generation was mutated: %v", err)
	}
	record, err := readPrivateQueueMaintenanceRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	if record.Requested != 1 || record.Attempted != 0 || record.Completed != 0 {
		t.Fatalf("incomplete generation consumed maintenance request: %+v", record)
	}
	if output := logOutput(); !strings.Contains(output, ErrPrivateSupervisorMigrationRequired.Error()) {
		t.Fatalf("incomplete generation cleanup log=%q, want migration deferral", output)
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
	fixture         privateSupervisorTestFixture
	client          *Client
	base            string
	startedPath     string
	releasePath     string
	commandObserved chan struct{}
	statusCommands  atomic.Int64
	removeCommands  atomic.Int64
	releaseOnce     sync.Once
}

type requestedPrivateMaintenanceResult struct {
	generation uint64
	started    bool
	err        error
}

func startRequestedPrivateMaintenanceContenders(
	ctx context.Context,
	client *Client,
	stateRoot string,
	contenderCount int,
) (<-chan requestedPrivateMaintenanceResult, <-chan struct{}) {
	start := make(chan struct{})
	results := make(chan requestedPrivateMaintenanceResult, contenderCount)
	var contenders sync.WaitGroup
	contenders.Add(contenderCount)
	for range contenderCount {
		go func() {
			defer contenders.Done()
			<-start
			generation, requestErr := requestPrivateQueueMaintenance(ctx, stateRoot)
			started := requestErr == nil && client.RunRequestedPrivateQueueMaintenance(ctx)
			results <- requestedPrivateMaintenanceResult{
				generation: generation,
				started:    started,
				err:        requestErr,
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		contenders.Wait()
		close(results)
		close(done)
	}()
	close(start)
	return results, done
}

func waitForRequestedPrivateMaintenanceContenders(
	t *testing.T,
	scenario *privateTerminalTriggerScenario,
	contendersDone <-chan struct{},
	ownerDone <-chan bool,
) {
	t.Helper()
	waitContext, cancelWait := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelWait()
	select {
	case <-contendersDone:
	case ownerStarted := <-ownerDone:
		scenario.releaseStatus(t)
		t.Fatalf("elected maintenance owner returned before status release: started=%t", ownerStarted)
	case <-waitContext.Done():
		scenario.releaseStatus(t)
		t.Fatalf("maintenance contenders waited behind the elected owner: %v", waitContext.Err())
	}
}

func assertRequestedPrivateMaintenanceContenders(
	t *testing.T,
	results <-chan requestedPrivateMaintenanceResult,
	firstGeneration, lastGeneration uint64,
) {
	t.Helper()
	seen := make(map[uint64]struct{}, int(lastGeneration-firstGeneration+1))
	for result := range results {
		if result.err != nil {
			t.Fatalf("publish maintenance generation: %v", result.err)
		}
		if !result.started {
			t.Fatalf("maintenance generation %d was not admitted", result.generation)
		}
		if result.generation < firstGeneration || result.generation > lastGeneration {
			t.Fatalf("maintenance generation=%d, want range [%d,%d]", result.generation, firstGeneration, lastGeneration)
		}
		if _, duplicate := seen[result.generation]; duplicate {
			t.Fatalf("maintenance generation %d was published more than once", result.generation)
		}
		seen[result.generation] = struct{}{}
	}
	want := int(lastGeneration - firstGeneration + 1)
	if len(seen) != want {
		t.Fatalf("published maintenance generations=%d, want %d", len(seen), want)
	}
}

func waitForRequestedPrivateMaintenanceOwner(t *testing.T, ownerDone <-chan bool) {
	t.Helper()
	waitContext, cancelWait := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelWait()
	select {
	case started := <-ownerDone:
		if !started {
			t.Fatal("elected maintenance owner was not admitted")
		}
	case <-waitContext.Done():
		t.Fatalf("elected maintenance owner did not finish after release: %v", waitContext.Err())
	}
}

func testPrivateTerminalTriggerCoalescing(t *testing.T, operation string, trigger func(*Client), wantStatus, wantRemove int64) {
	t.Helper()
	scenario := newPrivateTerminalTriggerScenario(t, operation, wantRemove != 0)
	runPrivateTerminalTriggerScenario(t, scenario, operation, trigger)
	if got := scenario.statusCommands.Load(); got != wantStatus {
		t.Fatalf("concurrent %s issued %d queue snapshots, want %d", operation, got, wantStatus)
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
		fixture:         fixture,
		client:          client,
		base:            base,
		startedPath:     filepath.Join(base, operation+"-status-started"),
		releasePath:     filepath.Join(base, operation+"-status-release"),
		commandObserved: make(chan struct{}, 1),
	}
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	client.options.Environment = append(os.Environ(), "PRIVATE_STATUS_STARTED="+scenario.startedPath, "PRIVATE_STATUS_RELEASE="+scenario.releasePath)
	client.options.Observer = scenario.observeCommand
	t.Cleanup(func() { scenario.releaseStatus(t) })
	return scenario
}

func privateTerminalTriggerCleanupAuthorizer(_ context.Context, _ string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
	plan := PrivateQueueCleanupPlan{RemoveIDs: make([]int64, 0, len(jobs))}
	for _, job := range jobs {
		if job.State == StateEnded {
			plan.RemoveIDs = append(plan.RemoveIDs, job.ID)
		}
	}
	return plan, nil
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
	select {
	case scenario.commandObserved <- struct{}{}:
	default:
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
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		authorizations++
		assertMaintenanceProbeAuthorization(t, fixture.stateRoot, client.Binding(), ctx.Err(), root, binding, jobs)
		return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
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
	if authorizations != 1 || commands.Load() != 3 {
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
