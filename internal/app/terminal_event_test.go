package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func waitForPrivateTerminalEventRetirement(t *testing.T, ctx context.Context, supervisor *pueue.Client, id int64) {
	t.Helper()
	admissionfixture.WaitForPrivateTerminalEventRetirement(t, ctx, supervisor, id)
}

func waitForPrivateQueueMaintenanceIdle(t *testing.T, ctx context.Context, supervisor *pueue.Client) {
	t.Helper()
	admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, ctx, supervisor)
}

func TestLaterTerminalCallbackRetriesDurablyAuthorizedCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(true))
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
	return admissionfixture.NewRecordedTerminalCleanupFixture(t, admissioncore.TerminalEvidence{})
}

func newTerminalGenerationWriteFault(t *testing.T, root string, supervisor *pueue.Client, jobID int64) *terminalGenerationWriteFault {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	if err := os.MkdirAll(filepath.Join(base, "terminal-events"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real queue row has already crossed submission's identity lease. The
	// synthetic row needs those stable locks before the unrelated write fault.
	for _, name := range []string{"queue-maintenance.lock", "queue-maintenance-state.lock", "queue-writer-intent.lock", "queue-identity-gate.lock", "queue-identity.lock"} {
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
	if !slices.ContainsFunc(command.Argv, admissionfixture.TerminalMaintenanceLabelFilter) {
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
	writeTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(true))
	eventPath := filepath.Join(store.Root, ".supervisor", "terminal-events", strconv.FormatInt(job.ID, 10)+".json")
	if err := os.MkdirAll(filepath.Dir(eventPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eventPath, 0o700); err != nil {
		t.Fatal(err)
	}
	scheduled := supervisorcore.RequestInspectionQueueMaintenance(t.Context(), supervisor, pueue.InspectionObservation{
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
