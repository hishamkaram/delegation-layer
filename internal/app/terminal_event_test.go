package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAwaitInspectionAcceptsPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if enabled, enabledErr := supervisor.PrivateTerminalEventsEnabled(ctx); enabledErr != nil || !enabled {
		t.Fatalf("terminal events enabled=%t err=%v", enabled, enabledErr)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
	if event, found, readErr := supervisor.ReadPrivateTerminalEvent(ctx, job.ID); readErr != nil || !found || !event.Succeeded {
		t.Fatalf("terminal event before await = %+v found=%t err=%v", event, found, readErr)
	}
	facts, err := awaitInspection(ctx, operation, supervisor)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	waitForTestFileContent(t, removals, "7\n")
	assertTerminalMaintenanceStatusCalls(t, statusCalls.Load(), unfilteredStatusCalls.Load(), 3)
	waitForPrivateTerminalEventRetirement(t, ctx, supervisor, job.ID)
}

func waitForPrivateTerminalEventRetirement(t *testing.T, ctx context.Context, supervisor *pueue.Client, id int64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, found, err := supervisor.ReadPrivateTerminalEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			waitForPrivateQueueMaintenanceIdle(t, ctx, supervisor)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("accepted terminal event was not retired: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForPrivateQueueMaintenanceIdle(t *testing.T, ctx context.Context, supervisor *pueue.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stateRoot := filepath.Dir(filepath.Dir(supervisor.Binding().ConfigPath))
	journalPath := filepath.Join(stateRoot, ".supervisor", "queue-maintenance.json")
	failMaintenanceWait := func(stage string, waitErr error) {
		journal, journalErr := os.ReadFile(journalPath)
		t.Fatalf("private queue maintenance %s: %v journal=%q journal_error=%v", stage, waitErr, journal, journalErr)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
		if err != nil {
			failMaintenanceWait("retry-state check failed", err)
		}
		if !pending {
			if err = pueue.WaitForPrivateQueueMaintenanceIdle(ctx, stateRoot); err != nil {
				failMaintenanceWait("owner handoff failed", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			failMaintenanceWait("did not become idle", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestLaterTerminalCallbackRetriesDurablyAuthorizedCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(true))
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	failOnce := filepath.Join(store.Root, ".supervisor", "fail-cleanup-remove-once")
	if err := os.WriteFile(failOnce, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{"FAKE_FAIL_CLEANUP_REMOVE_ONCE=" + failOnce}
	runTerminalEventMaintenanceForTest(t, store, job.ID, statusCalls, unfilteredStatusCalls, environment...)
	if _, err := os.Lstat(filepath.Join(store.Root, ".supervisor", "terminal-event-removals")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed cleanup reported a removal: %v", err)
	}
	if _, found, err := supervisor.ReadPrivateTerminalEvent(t.Context(), job.ID); err != nil || !found {
		t.Fatalf("failed remove lost retry callback state: found=%t err=%v", found, err)
	}
	// Cleanup keeps the event and inspection ticket until exact row removal, so
	// the next maintenance generation retries only this terminal row.
	runTerminalEventMaintenanceForTest(t, store, job.ID, statusCalls, unfilteredStatusCalls, environment...)
	waitForTestFileContent(t, filepath.Join(store.Root, ".supervisor", "terminal-event-removals"), "7\n")
	waitForPrivateTerminalEventRetirement(t, t.Context(), supervisor, job.ID)
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 4 || unfiltered != 4 {
		t.Fatalf("cleanup retry made status_calls=%d unfiltered_status_calls=%d, want two streamed authorization/revalidation pairs", total, unfiltered)
	}
}

func TestTerminalEventRetainedWakeRunsCleanupWhenGenerationWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission fault used to reject the generation write")
	}
	store, _, supervisor, job := newRecordedTerminalCleanupFixture(t)
	fault := newTerminalGenerationWriteFault(t, store.Root, supervisor, job.ID)
	if err := runTerminalEvent(
		runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"},
		fault.dependencies(),
	); err != nil {
		t.Fatal(err)
	}
	fault.assertConsumed(t, supervisor, job.ID)
}

type terminalGenerationWriteObservation struct {
	event pueue.TerminalEvent
	err   error
}

type terminalGenerationWriteFault struct {
	ctx                   context.Context
	base                  string
	baseMode              os.FileMode
	supervisor            *pueue.Client
	jobID                 int64
	observed              chan terminalGenerationWriteObservation
	observeOnce           sync.Once
	restored              atomic.Bool
	statusCalls           atomic.Int32
	unfilteredStatusCalls atomic.Int32
}

func newRecordedTerminalCleanupFixture(t *testing.T) (*taskdir.Store, *inspection.Operation, *pueue.Client, pueue.Job) {
	t.Helper()
	store := newBareInspectionStore(t)
	supervisor, _, _ := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(true))
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	return store, operation, supervisor, job
}

func newTerminalGenerationWriteFault(t *testing.T, root string, supervisor *pueue.Client, jobID int64) *terminalGenerationWriteFault {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	if err := os.MkdirAll(filepath.Join(base, "terminal-events"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"queue-maintenance.lock", "queue-maintenance-state.lock"} {
		createTerminalGenerationLock(t, filepath.Join(base, name))
	}
	baseInfo, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	fault := &terminalGenerationWriteFault{
		ctx:        t.Context(),
		base:       base,
		baseMode:   baseInfo.Mode().Perm(),
		supervisor: supervisor,
		jobID:      jobID,
		observed:   make(chan terminalGenerationWriteObservation, 1),
	}
	if err = os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !fault.restored.Load() {
			if restoreErr := fault.restore(); restoreErr != nil {
				t.Errorf("restore private supervisor directory permissions: %v", restoreErr)
			}
		}
	})
	return fault
}

func createTerminalGenerationLock(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func (fault *terminalGenerationWriteFault) dependencies() Dependencies {
	deps := terminalEventMaintenanceDependencies(nil, nil)
	deps.SupervisorOptions.Observer = fault.observeStatus
	return deps
}

func (fault *terminalGenerationWriteFault) observeStatus(command pueue.CommandEvent) {
	if command.Stage != "entry" || !slices.Contains(command.Argv, "status") {
		return
	}
	fault.statusCalls.Add(1)
	if !slices.ContainsFunc(command.Argv, terminalMaintenanceLabelFilter) {
		fault.unfilteredStatusCalls.Add(1)
	}
	fault.observeOnce.Do(func() {
		readContext, cancelRead := context.WithTimeout(fault.ctx, pueue.DefaultObservationTimeout)
		event, found, readErr := fault.supervisor.ReadPrivateTerminalEvent(readContext, fault.jobID)
		cancelRead()
		if readErr == nil && !found {
			readErr = errors.New("retained terminal event was absent before cleanup")
		}
		fault.observed <- terminalGenerationWriteObservation{
			event: event,
			err:   errors.Join(readErr, fault.restore()),
		}
	})
}

func (fault *terminalGenerationWriteFault) restore() error {
	err := os.Chmod(fault.base, fault.baseMode)
	if err == nil {
		fault.restored.Store(true)
	}
	return err
}

func (fault *terminalGenerationWriteFault) assertConsumed(t *testing.T, supervisor *pueue.Client, jobID int64) {
	t.Helper()
	select {
	case observation := <-fault.observed:
		if observation.err != nil {
			t.Fatal(observation.err)
		}
		if observation.event.NumericTaskID != jobID || !observation.event.Succeeded {
			t.Fatalf("retained terminal event before cleanup = %+v", observation.event)
		}
	default:
		t.Fatal("retained terminal event did not reach cleanup")
	}
	waitForTestFileContent(t, filepath.Join(fault.base, "terminal-event-removals"), "7\n")
	waitForPrivateTerminalEventRetirement(t, t.Context(), supervisor, jobID)
	requested := readTerminalMaintenanceRequested(t, filepath.Join(fault.base, "queue-maintenance.json"))
	if requested != 0 {
		t.Fatalf("failed generation write advanced requested generation to %d", requested)
	}
	if total, unfiltered := fault.statusCalls.Load(), fault.unfilteredStatusCalls.Load(); total != 2 || unfiltered != 2 {
		t.Fatalf("retained-event cleanup made status_calls=%d unfiltered_status_calls=%d, want one streamed authorization/revalidation pair", total, unfiltered)
	}
}

func readTerminalMaintenanceRequested(t *testing.T, path string) uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Requested uint64 `json:"requested"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record.Requested
}

func TestTerminalEventRetainsMaintenanceRequestWhenBindingCannotOpen(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	bindingPath := filepath.Join(store.Root, ".supervisor", "supervisor.identity.json")
	if err := os.WriteFile(bindingPath, []byte("{\"invalid\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTerminalEvent(
		runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"},
		terminalEventMaintenanceDependencies(nil, nil),
	); err != nil {
		t.Fatal(err)
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(t.Context(), store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("binding failure consumed the durable maintenance request")
	}
}

func TestTerminalEventCandidateFailureStillPublishesMaintenanceRequest(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(store.Root, ".supervisor", "terminal-events", strconv.FormatInt(job.ID, 10)+".json")
	if err := os.MkdirAll(filepath.Dir(eventPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// Keep the owner from consuming the request. The assertion below isolates
	// ordering: candidate publication may fail, but its conservative wake must
	// already be durable before supervisor recovery is attempted.
	bindingPath := filepath.Join(store.Root, ".supervisor", "supervisor.identity.json")
	if err := os.WriteFile(bindingPath, []byte("{\"invalid\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTerminalEvent(
		runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"},
		terminalEventMaintenanceDependencies(nil, nil),
	); err != nil {
		t.Fatal(err)
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(t.Context(), store.Root)
	if err != nil || !pending {
		t.Fatalf("candidate failure cleanup request pending=%t err=%v", pending, err)
	}
}

func TestInspectionObservationCandidateFailureRequestsFullMaintenance(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(true))
	eventPath := filepath.Join(store.Root, ".supervisor", "terminal-events", strconv.FormatInt(job.ID, 10)+".json")
	if err := os.MkdirAll(filepath.Dir(eventPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventPath, 0o700); err != nil {
		t.Fatal(err)
	}
	scheduled := requestInspectionQueueMaintenance(t.Context(), supervisor, pueue.InspectionObservation{
		Binding:  supervisor.Binding(),
		Identity: operation.Request().SupervisorIdentity(),
		Job:      job,
		Matched:  true,
	})
	if !scheduled {
		t.Fatal("candidate failure did not schedule its durable cleanup owner")
	}
	idleContext, cancelIdle := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancelIdle()
	if err := pueue.WaitForPrivateQueueMaintenanceIdle(idleContext, store.Root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root, ".supervisor", "queue-maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Requested     uint64 `json:"requested"`
		FullRequested uint64 `json:"full_requested"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Requested == 0 || record.FullRequested != record.Requested {
		t.Fatalf("candidate failure maintenance state=%+v, want latest request promoted to full scope", record)
	}
	if _, found, inboxErr := supervisor.ReadPrivateTerminalEvent(idleContext, job.ID); found || !errors.Is(inboxErr, pueue.ErrConfiguration) {
		t.Fatalf("malformed terminal inbox result found=%t err=%v, want explicit configuration error", found, inboxErr)
	}
}

func TestOrdinaryTerminalCallbackRequestsFullQueueMaintenance(t *testing.T) {
	store := newBareInspectionStore(t)
	_, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)

	// No inspection ticket exists for this numeric ID. The callback must still
	// trigger ordinary-task cleanup, and the empty fixture makes one full
	// authorization snapshot sufficient.
	runTerminalEventMaintenanceForTest(t, store, 41, statusCalls, unfilteredStatusCalls)
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 1 || unfiltered != 1 {
		t.Fatalf("ordinary callback made status_calls=%d unfiltered_status_calls=%d, want one streamed cleanup observation", total, unfiltered)
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(t.Context(), store.Root)
	if err != nil || pending {
		t.Fatalf("ordinary callback cleanup pending=%t err=%v, want completed", pending, err)
	}
}

func TestConcurrentCompletedInspectionsUseExactQueueObservations(t *testing.T) {
	store := newBareInspectionStore(t)
	// Publish both wakes before running maintenance so exact batch counts do
	// not depend on the scheduler's bounded quiet window.
	var requestedLaunches atomic.Int32
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithLauncher(t, store, func(context.Context, string, string) error {
		requestedLaunches.Add(1)
		return nil
	})
	// Fixture journals are fsync-heavy and created serially before observation.
	// Two operations prove concurrent exact reconciliation deterministically;
	// the native private-CLI acceptance proves 200-task production capacity.
	const count = 2
	operations := make([]*inspection.Operation, 0, count)
	jobs := make([]pueue.Job, 0, count)
	for index := range count {
		taskID := fmt.Sprintf("%032x", index+1)
		operation, job := newTerminalInspectionCleanupOperationForTask(
			t, store, supervisor, inspection.ResultEligible, true, taskID, int64(index+7),
		)
		completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
		if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
		if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
		jobs = append(jobs, job)
	}
	writeTerminalEventStatuses(t, store, jobs)

	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	type result struct {
		taskID string
		facts  []byte
		err    error
	}
	results := make(chan result, count)
	for _, operation := range operations {
		go func() {
			facts, err := awaitInspection(ctx, operation, supervisor)
			results <- result{taskID: operation.Request().TaskID, facts: facts, err: err}
		}()
	}
	for range count {
		select {
		case observed := <-results:
			if observed.err != nil || string(observed.facts) != "{\"eligible\":true}\n" {
				t.Fatalf("exact admission task=%s facts=%s err=%v status_calls=%d unfiltered_status_calls=%d", observed.taskID, observed.facts, observed.err, statusCalls.Load(), unfilteredStatusCalls.Load())
			}
		case <-ctx.Done():
			t.Fatalf("coalesced admissions did not finish: %v", ctx.Err())
		}
	}
	runInspectionMaintenanceBatch(t, ctx, supervisor, requestedLaunches.Load(), count)
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	removed, err := os.ReadFile(removals)
	if err != nil {
		t.Fatal(err)
	}
	removedIDs := strings.Fields(string(removed))
	slices.Sort(removedIDs)
	if !slices.Equal(removedIDs, []string{"7", "8"}) {
		t.Fatalf("concurrent exact cleanup removals=%v, want [7 8]", removedIDs)
	}
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != count*2+2 || unfiltered != 2 {
		t.Fatalf("%d completed inspections made status_calls=%d unfiltered_status_calls=%d, want one admission observation and one candidate revalidation per inspection plus one streamed cleanup authorization/revalidation pair", count, total, unfiltered)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func runInspectionMaintenanceBatch(t *testing.T, ctx context.Context, supervisor *pueue.Client, launches, want int32) {
	t.Helper()
	if launches != want {
		t.Fatalf("maintenance launch requests=%d, want %d", launches, want)
	}
	if !supervisor.RunRequestedPrivateQueueMaintenance(ctx) {
		t.Fatal("persisted maintenance did not start")
	}
	waitForPrivateQueueMaintenanceIdle(t, ctx, supervisor)
}

func TestAwaitInspectionDoesNotWaitForBlockedPrivateCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	blockPath := filepath.Join(store.Root, ".supervisor", "block-cleanup-remove")
	releasePath := filepath.Join(store.Root, ".supervisor", "release-cleanup-remove")
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(
		t,
		store,
		"FAKE_BLOCK_CLEANUP_REMOVE="+blockPath,
		"FAKE_RELEASE_CLEANUP_REMOVE="+releasePath,
	)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	released := false
	defer func() {
		if !released {
			if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}()

	seedBlockedTerminalInspectionEvent(t, ctx, store, blockPath, supervisor, operation, job)
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- runTerminalEvent(runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"}, terminalEventMaintenanceDependencies(
			statusCalls,
			unfilteredStatusCalls,
			"FAKE_BLOCK_CLEANUP_REMOVE="+blockPath,
			"FAKE_RELEASE_CLEANUP_REMOVE="+releasePath,
		))
	}()
	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-cleanup-blocked"), 15*time.Second)
	type result struct {
		facts []byte
		err   error
	}
	results := make(chan result, 1)
	go func() {
		facts, err := awaitInspection(ctx, operation, supervisor)
		results <- result{facts: facts, err: err}
	}()

	admissionTimer := time.NewTimer(15 * time.Second)
	defer admissionTimer.Stop()
	select {
	case observed := <-results:
		if observed.err != nil || string(observed.facts) != "{\"eligible\":true}\n" {
			t.Fatalf("terminal event admission facts=%s err=%v", observed.facts, observed.err)
		}
	case <-admissionTimer.C:
		t.Fatal("admission waited for root-wide cleanup")
	case <-ctx.Done():
		t.Fatalf("admission context ended before blocked cleanup returned: %v", ctx.Err())
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released = true
	if callbackErr := <-callbackDone; callbackErr != nil {
		t.Fatal(callbackErr)
	}
	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-removals"), 15*time.Second)
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 2 || unfiltered != 2 {
		t.Fatalf("blocked callback cleanup made status_calls=%d unfiltered_status_calls=%d, want one streamed authorization/revalidation pair and no admission-side cleanup observation", total, unfiltered)
	}
}

func TestTerminalInspectionWakeRetriesConcurrentFailedCleanupWithoutAnotherCallback(t *testing.T) {
	store := newBareInspectionStore(t)
	base := filepath.Join(store.Root, ".supervisor")
	blockPath := filepath.Join(base, "block-cleanup-status")
	releasePath := filepath.Join(base, "release-cleanup-status")
	failOnce := filepath.Join(base, "fail-cleanup-remove-once")
	environment := []string{
		"FAKE_BLOCK_CLEANUP_STATUS=" + blockPath,
		"FAKE_RELEASE_CLEANUP_STATUS=" + releasePath,
		"FAKE_FAIL_CLEANUP_REMOVE_ONCE=" + failOnce,
	}
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, path := range []string{blockPath, failOnce} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	released := false
	defer func() {
		if !released {
			if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- runTerminalEvent(
			runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"},
			terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls, environment...),
		)
	}()
	waitForTestPathWithin(t, filepath.Join(base, "terminal-event-status-blocked"), 15*time.Second)
	facts, err := awaitInspection(ctx, operation, supervisor)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("terminal inspection facts=%s err=%v", facts, err)
	}
	if err = os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released = true
	if callbackErr := <-callbackDone; callbackErr != nil {
		t.Fatal(callbackErr)
	}
	waitForTestFileContent(t, filepath.Join(base, "terminal-event-removals"), "7\n")
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	if _, err = os.Lstat(failOnce); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first failed removal was not consumed: %v", err)
	}
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 5 || unfiltered != 4 {
		t.Fatalf("failed-pass retry made status_calls=%d unfiltered_status_calls=%d, want admission reconciliation and two streamed authorization/revalidation pairs", total, unfiltered)
	}
}

func seedBlockedTerminalInspectionEvent(t *testing.T, ctx context.Context, store *taskdir.Store, blockPath string, supervisor *pueue.Client, operation *inspection.Operation, job pueue.Job) {
	t.Helper()
	if err := os.WriteFile(blockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitInspectionRejectsFailedPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job.Succeeded = false
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if enabled, enabledErr := supervisor.PrivateTerminalEventsEnabled(ctx); enabledErr != nil || !enabled {
		t.Fatalf("terminal events enabled=%t err=%v", enabled, enabledErr)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
	if event, found, readErr := supervisor.ReadPrivateTerminalEvent(ctx, job.ID); readErr != nil || !found || !event.Succeeded {
		t.Fatalf("successful callback hint for failed row = %+v found=%t err=%v", event, found, readErr)
	}
	facts, err := awaitInspection(ctx, operation, supervisor)
	if facts != nil || !errors.Is(err, errInspectionWorkerUnavailable) {
		t.Fatalf("failed terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	waitForTestPath(t, removals)
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	assertTerminalMaintenanceStatusCalls(t, statusCalls.Load(), unfilteredStatusCalls.Load(), 3)
	if _, succeeded, readErr := operation.ReadWorkerResultContext(ctx); readErr != nil || succeeded {
		t.Fatalf("failed terminal event did not persist failed worker observation: succeeded=%t err=%v", succeeded, readErr)
	}
}

func assertTerminalMaintenanceStatusCalls(t *testing.T, statusCalls, unfilteredStatusCalls, wantTotal int32) {
	t.Helper()
	if statusCalls != wantTotal || unfilteredStatusCalls != wantTotal-1 {
		t.Fatalf("completed inspection made status_calls=%d unfiltered_status_calls=%d, want %d total with one streamed cleanup authorization/revalidation pair", statusCalls, unfilteredStatusCalls, wantTotal)
	}
}

func TestAwaitInspectionFallsBackToExactRowWhenTerminalEventRegistrationOrCallbackFails(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		terminalEventRegistered        bool
		terminalEventRegistrationKnown bool
	}{{
		name:                           "registration failure",
		terminalEventRegistrationKnown: true,
	}, {
		name:                           "callback failure",
		terminalEventRegistrationKnown: true,
		terminalEventRegistered:        true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			testInspectionTerminalEventFallback(t, tc.terminalEventRegistrationKnown, tc.terminalEventRegistered)
		})
	}
}

func testInspectionTerminalEventFallback(t *testing.T, registrationKnown, registered bool) {
	t.Helper()
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if registered {
		if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor, inspectionSubmissionState{
		terminalEventRegistrationKnown: registrationKnown,
		terminalEventRegistered:        registered,
	})
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("exact terminal-row fallback facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	removed, readErr := os.ReadFile(removals)
	if readErr != nil {
		t.Fatalf("private queue maintenance completed without removal record: %v", readErr)
	}
	if string(removed) != "7\n" {
		t.Fatalf("fallback removal contents=%q, want %q", removed, "7\n")
	}
	if got := statusCalls.Load(); got != 4 {
		t.Fatalf("fallback made %d status calls, want one exact reconciliation, one candidate revalidation and one cleanup authorization/revalidation pair", got)
	}
}

func runTerminalEventMaintenanceForTest(t *testing.T, store *taskdir.Store, id int64, statusCalls, unfilteredStatusCalls *atomic.Int32, environment ...string) {
	t.Helper()
	err := runTerminalEvent(
		runnerArguments{Root: store.Root, PueueID: id, ExitCode: "0"},
		terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls, environment...),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls *atomic.Int32, environment ...string) Dependencies {
	options := pueue.Options{
		ObservationTimeout: inspection.AdmissionTimeout,
		Environment:        append(os.Environ(), environment...),
	}
	if statusCalls != nil {
		options.Observer = func(event pueue.CommandEvent) {
			if event.Stage != "entry" || !slices.Contains(event.Argv, "status") {
				return
			}
			statusCalls.Add(1)
			if unfilteredStatusCalls != nil && !slices.ContainsFunc(event.Argv, terminalMaintenanceLabelFilter) {
				unfilteredStatusCalls.Add(1)
			}
		}
	}
	return Dependencies{SupervisorOptions: options}.normalized()
}

func terminalMaintenanceLabelFilter(argument string) bool {
	return strings.HasPrefix(argument, "label=") || strings.HasPrefix(argument, "label%=")
}

func waitForTestFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.NewTimer(pueue.DefaultObservationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if last == want {
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s content: got=%q want=%q", path, last, want)
		case <-ticker.C:
		}
	}
}

func TestDelayedTerminalEventAfterNumericIDReuseCannotAuthorizeNewInspection(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	requireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	requireAppTest(t, supervisor.ClearPrivateTerminalEvents(ctx, job.ID))
	requireAppTest(t, operation.RecordReceiptContext(ctx, job.ID))
	writeTerminalEventStatus(t, store, job, terminalEventRunningState())
	requireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	requireAppTest(t, pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"))
	replacement := job
	replacementLabel := "replacement-terminal-row"
	replacement.Label = &replacementLabel
	replacement.State = pueue.StateEnded
	replacement.Succeeded = true
	writeTerminalEventStatus(t, store, replacement, terminalEventDoneState(replacement.Succeeded))

	if event, found, readErr := supervisor.ReadPrivateTerminalEvent(ctx, job.ID); readErr != nil || !found || !event.Succeeded {
		t.Fatalf("delayed callback hint = %+v found=%t err=%v", event, found, readErr)
	}
	state := inspectionAwaitState{
		identity:                       operation.Request().SupervisorIdentity(),
		polls:                          inspectionPollSchedule{callbackExpected: true},
		terminalEvents:                 true,
		terminalEventRegistrationKnown: true,
		terminalEventRegistered:        true,
	}
	state.identity.NumericTaskID = &job.ID
	for attempt := range 2 {
		facts, observed, err := state.observe(ctx, operation, supervisor)
		if err != nil || observed || facts != nil {
			t.Fatalf("delayed predecessor callback attempt=%d authorized replacement: facts=%s observed=%t err=%v", attempt, facts, observed, err)
		}
	}
	if got := statusCalls.Load(); got != 1 {
		t.Fatalf("latched delayed callback made %d exact observations, want 1", got)
	}
	if _, _, readErr := operation.ReadWorkerResultContext(ctx); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("delayed predecessor callback published worker authority: %v", readErr)
	}
	if facts, recorded, terminal, readErr := recordedInspectionFacts(ctx, operation); facts != nil || recorded || terminal || readErr != nil {
		t.Fatalf("delayed predecessor callback granted admission authority: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, readErr)
	}
}

func TestSubmitInspectionOnceUsesConcurrentAddReceiptWithoutQueueSnapshot(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err = filepath.EvalSymlinks(runner)
	if err != nil {
		t.Fatal(err)
	}
	runnerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	request := newBareInspectionRequest(t, store)
	operation, err := inspection.OpenConcurrentOperation(store, request, inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   task.ComputeSHA256([]byte("runtime-definition")),
		CapabilitySHA256:   task.ComputeSHA256([]byte("runtime-capability")),
		HelperExecutable:   runner,
		HelperSHA256:       runnerSHA,
		WorkerExecutable:   runner,
		WorkerSHA256:       runnerSHA,
		Supervisor:         supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	assertTerminalEventInspectionGenerationReady(t, store, supervisor, ctx)
	if err = submitInspectionOnce(ctx, operation, supervisor, runner, store.Root); err != nil {
		t.Fatal(err)
	}
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 9 {
		t.Fatalf("concurrent inspection receipt = %+v err=%v", receipt, err)
	}
	if calls := statusCalls.Load(); calls != 0 {
		t.Fatalf("concurrent submission made %d queue-wide status calls", calls)
	}
}

func newTerminalEventInspectionSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, *atomic.Int32) {
	client, statusCalls, _ := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	return client, statusCalls
}

func newTerminalEventInspectionSupervisorWithEnvironment(t *testing.T, store *taskdir.Store, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return newTerminalEventInspectionSupervisorWithLauncher(t, store, nil, extraEnvironment...)
}

func newTerminalEventInspectionSupervisorWithLauncher(t *testing.T, store *taskdir.Store, launcher func(context.Context, string, string) error, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	bundle := canonicalAppTestTempDir(t)
	clientPath := filepath.Join(bundle, "pueue")
	daemonPath := filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    if [ "${FAKE_BLOCK_CLEANUP_STATUS:-}" != "" ] && [ -f "$FAKE_BLOCK_CLEANUP_STATUS" ]; then
      : > "$base/terminal-event-status-blocked"
      while [ ! -f "${FAKE_RELEASE_CLEANUP_STATUS:?}" ]; do sleep 0.01; done
    fi
    if [ -f "$base/terminal-event-status.json" ]; then
      cat "$base/terminal-event-status.json"
      exit 0
    fi
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s}}}\n' "$parallel"
    ;;
  group)
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"default":{"status":"Running","parallel_tasks":%s}}\n' "$parallel"
    ;;
  parallel) : > "$base/parallel-unlimited" ;;
  remove)
	if [ -f "$base/block-maintenance-remove" ]; then
	  : > "$base/maintenance-remove-blocked"
	  while [ ! -f "$base/release-maintenance-remove" ]; do sleep 0.01; done
	fi
    if [ "${FAKE_BLOCK_CLEANUP_REMOVE:-}" != "" ] && [ -f "$FAKE_BLOCK_CLEANUP_REMOVE" ]; then
      : > "$base/terminal-event-cleanup-blocked"
      while [ ! -f "${FAKE_RELEASE_CLEANUP_REMOVE:?}" ]; do sleep 0.01; done
    fi
	if [ "${FAKE_FAIL_CLEANUP_REMOVE_ONCE:-}" != "" ] && [ -f "$FAKE_FAIL_CLEANUP_REMOVE_ONCE" ]; then
	  rm -f "$FAKE_FAIL_CLEANUP_REMOVE_ONCE"
	  exit 75
	fi
    shift 3
    printf '%s\n' "$*" >> "$base/terminal-event-removals"
    ;;
  add)
    if [ "${4-}" = --help ]; then printf '%s\n' '--stashed --print-task-id'; else printf '%s\n' '9'; fi
    ;;
  enqueue)
    if [ "${4-}" = --help ]; then printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'; fi
    ;;
  *) exit 64 ;;
esac
`)
	writeRunnerFixture(t, daemonPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
`)
	var statusCalls atomic.Int32
	var unfilteredStatusCalls atomic.Int32
	options := pueue.Options{
		ObservationTimeout:          inspection.AdmissionTimeout,
		SuccessfulCleanupAuthorizer: authorizePrivateSuccessfulCleanup,
		PrivateMaintenanceLauncher:  launcher,
		Environment:                 append(os.Environ(), extraEnvironment...),
		Observer: func(event pueue.CommandEvent) {
			if event.Stage == "entry" && slices.Contains(event.Argv, "status") {
				statusCalls.Add(1)
				if !slices.ContainsFunc(event.Argv, terminalMaintenanceLabelFilter) {
					unfilteredStatusCalls.Add(1)
				}
			}
		},
	}
	bootstrap, err := pueue.BindPrivate(t.Context(), clientPath, daemonPath, store.Root, options)
	if err != nil {
		t.Fatal(err)
	}
	listenSupervisorProbeTestEndpoint(t, bootstrap.Binding().Endpoint)
	client, err := preparePrivateAdmissionSupervisor(Dependencies{
		InitialSupervisorExecutable: clientPath,
		SupervisorOptions:           options,
	}, store, options)
	settleSupervisorProbeTestClient(t, client, err)
	if closeErr := bootstrap.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	statusCalls.Store(0)
	unfilteredStatusCalls.Store(0)
	for _, name := range []string{"terminal-event-cleanup-blocked", "terminal-event-removals"} {
		if removeErr := os.Remove(filepath.Join(store.Root, ".supervisor", name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Fatal(removeErr)
		}
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, &statusCalls, &unfilteredStatusCalls
}

func assertTerminalEventInspectionGenerationReady(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, ctx context.Context) {
	t.Helper()
	complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, supervisor.Binding())
	if completeErr != nil || !complete {
		migration, migrationErr := pueue.PrivateSupervisorMigrationPending(store.Root)
		recovery, recoveryErr := pueue.PrivateRecoveryBindingPresent(store.Root)
		bootstrap, bootstrapErr := pueue.PrivateBootstrapPending(store.Root)
		daemon, daemonErr := pueue.PrivateDaemonIdentityPresent(store.Root)
		entries, readErr := os.ReadDir(filepath.Join(store.Root, ".supervisor"))
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("private generation incomplete: complete=%t err=%v migration=%t/%v recovery=%t/%v bootstrap=%t/%v daemon=%t/%v files=%v read=%v binding=%+v runtime=%+v", complete, completeErr, migration, migrationErr, recovery, recoveryErr, bootstrap, bootstrapErr, daemon, daemonErr, names, readErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	recorded, recordErr := privateOrdinarySchedulingRecorded(store.Root, supervisor.Binding())
	if recordErr != nil || !recorded {
		t.Fatalf("private scheduling unrecorded: recorded=%t err=%v binding=%+v runtime=%+v", recorded, recordErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	if _, gateErr := privateSubmissionGate(store.Root)(ctx, supervisor.Binding()); gateErr != nil {
		t.Fatalf("submission gate: %v", gateErr)
	}
}

func writeTerminalEventStatus(t *testing.T, store *taskdir.Store, job pueue.Job, state map[string]any) {
	t.Helper()
	if job.Label == nil {
		t.Fatal("terminal-event status requires a job label")
	}
	parallelTasks := uint64(1)
	if job.Group == "default" {
		parallelTasks = 0
	}
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(job.ID, 10): map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": state,
		}},
		"groups": map[string]any{job.Group: map[string]any{"status": "Running", "parallel_tasks": parallelTasks}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "terminal-event-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTerminalEventStatuses(t *testing.T, store *taskdir.Store, jobs []pueue.Job) {
	t.Helper()
	tasks := make(map[string]any, len(jobs))
	groups := make(map[string]any)
	for _, job := range jobs {
		if job.Label == nil {
			t.Fatal("terminal-event status requires a job label")
		}
		parallelTasks := uint64(1)
		if job.Group == "default" {
			parallelTasks = 0
		}
		tasks[strconv.FormatInt(job.ID, 10)] = map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": terminalEventDoneState(job.Succeeded),
		}
		groups[job.Group] = map[string]any{"status": "Running", "parallel_tasks": parallelTasks}
	}
	status, err := json.Marshal(map[string]any{"tasks": tasks, "groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "terminal-event-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
}

func terminalEventDoneState(succeeded bool) map[string]any {
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	return map[string]any{"Done": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z", "end": "2026-10-10T00:00:00Z", "result": result,
	}}
}

func terminalEventRunningState() map[string]any {
	return map[string]any{"Running": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z",
	}}
}
