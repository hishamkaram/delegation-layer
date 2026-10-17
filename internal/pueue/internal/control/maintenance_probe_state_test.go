package control

import (
	"context"
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
	leaseEntered := make(chan struct{})
	leaseRelease := make(chan struct{})
	var authorizations atomic.Int64
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, _ string, _ task.SupervisorRef, _ []Job) (PrivateQueueCleanupPlan, error) {
		if authorizations.Add(1) == 1 {
			close(firstAuthorization)
			select {
			case <-leaseEntered:
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

	batch := startRequestedPrivateMaintenanceContenders(
		ctx,
		scenario.client,
		scenario.fixture.stateRoot,
		contenderCount,
	)
	defer batch.release()
	waitForRequestedPrivateMaintenanceRequests(t, scenario, batch.requestsDone, ownerDone)
	batch.release()
	waitForRequestedPrivateMaintenanceContenders(t, scenario, batch.contendersDone, ownerDone)
	assertRequestedPrivateMaintenanceContenders(t, batch.results, 2, contenderCount+1)
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
	// Snapshot preparation now intentionally owns queue identity. Block the
	// actual authorizer, rather than mislabelling a blocked status subprocess.
	scenario.releaseStatus(t)
	authorizationEntered, authorizationRelease := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseAuthorization := func() { releaseOnce.Do(func() { close(authorizationRelease) }) }
	defer releaseAuthorization()
	scenario.client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		enteredOnce.Do(func() { close(authorizationEntered) })
		select {
		case <-ctx.Done():
			return PrivateQueueCleanupPlan{}, ctx.Err()
		case <-authorizationRelease:
			return privateTerminalTriggerCleanupAuthorizer(ctx, root, binding, jobs)
		}
	}
	if !scenario.client.SchedulePrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not scheduled")
	}
	select {
	case <-authorizationEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("private maintenance did not reach authorization")
	}

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

	releaseAuthorization()
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
		{name: "full", request: RequestPrivateQueueMaintenance, wantIDs: []int64{0, 1, 2}},
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
