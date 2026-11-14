package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// privateCustodyRecorder keeps every scope handed to the custody port and the
// first fault observed inside it. The port runs on the maintenance goroutine,
// so assertions happen after the pass returns.
type privateCustodyRecorder struct {
	mu     sync.Mutex
	scopes []PrivateTerminalCustodyScope
	fault  error
}

func (r *privateCustodyRecorder) record(scope PrivateTerminalCustodyScope, fault error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes = append(r.scopes, scope)
	r.fault = errors.Join(r.fault, fault)
}

func (r *privateCustodyRecorder) snapshot() ([]PrivateTerminalCustodyScope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PrivateTerminalCustodyScope(nil), r.scopes...), r.fault
}

// enterPrivateLockWithin proves that a lock is free now. A held lock turns the
// bounded wait into a deadline fault instead of blocking the pass forever.
func enterPrivateLockWithin(ctx context.Context, acquire func(context.Context, func(context.Context) error) error) error {
	lockContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	entered := false
	err := acquire(lockContext, func(context.Context) error {
		entered = true
		return nil
	})
	if err == nil && !entered {
		return errSupervisorLockBusy
	}
	return err
}

func readPrivateMaintenanceRecordForTest(t *testing.T, stateRoot string) privateQueueMaintenanceRecord {
	t.Helper()
	record, err := readPrivateQueueMaintenanceRecord(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// runRequestedPrivateMaintenanceForTest publishes one wake through the given
// journal operation and runs the elected owner to completion in this process.
func runRequestedPrivateMaintenanceForTest(t *testing.T, client *Client, stateRoot string, request func(context.Context, string) (uint64, error)) privateQueueMaintenanceRecord {
	t.Helper()
	if _, err := request(t.Context(), stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.RunRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not run")
	}
	return readPrivateMaintenanceRecordForTest(t, stateRoot)
}

func requirePrivateRowRemoved(t *testing.T, stateRoot string, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(stateRoot, privateSupervisorDirectory, "test-kill-task-id"))
	if want && err != nil {
		t.Fatalf("authorized row was not removed: %v", err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("row removal crossed a refused revalidation: %v", err)
	}
}

// A blocked custody owner must not hold the generation lease or queue
// identity: a stop or migration (bootstrap EX) and a submission (identity
// lease) both enter while it runs, and the same pass still removes the row.
func TestPrivateMaintenanceCustodyHoldsNoGenerationOrIdentityLease(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	recorder := &privateCustodyRecorder{}
	client.options.PrivateTerminalCustodyReleaser = func(ctx context.Context, root string, binding task.SupervisorRef, scope PrivateTerminalCustodyScope) error {
		var fault error
		fullPhase := scope.Full && len(scope.Candidates) == 0
		capturedPhase := !scope.Full && len(scope.Candidates) == 1 && scope.Candidates[0].ID == 0
		if root != fixture.stateRoot || binding != client.Binding() || (!fullPhase && !capturedPhase) {
			fault = errors.New("custody port lost its root, binding, or phase scope")
		}
		exclusive := enterPrivateLockWithin(ctx, func(lockContext context.Context, callback func(context.Context) error) error {
			return WithBootstrapLockContext(lockContext, root, callback)
		})
		submission := enterPrivateLockWithin(ctx, client.withPrivateQueueIdentityLease)
		_, removed := os.Lstat(filepath.Join(root, privateSupervisorDirectory, "test-kill-task-id"))
		if !errors.Is(removed, os.ErrNotExist) {
			fault = errors.Join(fault, errors.New("row removal preceded custody release"))
		}
		recorder.record(scope, errors.Join(fault, exclusive, submission))
		return nil
	}

	startPrivateMaintenancePass(t, client, func() {}).requireCompleted(t)
	scopes, fault := recorder.snapshot()
	if len(scopes) != 2 || !scopes[0].Full || scopes[1].Full || fault != nil {
		t.Fatalf("custody scopes=%+v fault=%v, want lock-free full then captured phases", scopes, fault)
	}
	requirePrivateRowRemoved(t, fixture.stateRoot, true)
}

// The elected owner releases the generation lease between protected phases.
// A bootstrap EX waiter queued during the first generation enters before the
// second generation settles, and holds while the first attempt is recorded.
// The quiet settle window, not lock fairness, gives the waiter its turn.
func TestScheduledPrivateMaintenanceReleasesGenerationLeaseWhileSettling(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	var authorizations atomic.Int32
	entry := make(chan privateQueueMaintenanceRecord, 1)
	exclusiveDone := make(chan error, 1)
	testContext := t.Context()
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		if authorizations.Add(1) == 1 {
			if _, err := requestPrivateCandidateQueueMaintenance(ctx, root); err != nil {
				return PrivateQueueCleanupPlan{}, err
			}
			go func(waitContext context.Context) {
				exclusiveDone <- WithBootstrapLockContext(waitContext, root, func(lockContext context.Context) error {
					return holdUntilFirstAttemptSettled(lockContext, root, entry)
				})
			}(testContext)
		}
		return privateTerminalTriggerCleanupAuthorizer(ctx, root, binding, jobs)
	}
	if _, err := requestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if !client.RunRequestedPrivateQueueMaintenance(t.Context()) {
		t.Fatal("private maintenance was not run")
	}
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	if seen := <-entry; seen.Requested != 2 || seen.Attempted >= 2 {
		t.Fatalf("bootstrap EX entered at %+v, want before the second generation settled", seen)
	}
	record := readPrivateMaintenanceRecordForTest(t, fixture.stateRoot)
	if authorizations.Load() != 2 || record.Requested != 2 || record.Attempted != 2 || record.Completed != 2 {
		t.Fatalf("authorizations=%d state=%+v, want both generations completed", authorizations.Load(), record)
	}
}

func holdUntilFirstAttemptSettled(ctx context.Context, root string, entry chan<- privateQueueMaintenanceRecord) error {
	base := filepath.Join(root, privateSupervisorDirectory)
	record, err := readPrivateQueueMaintenanceRecord(base)
	if err != nil {
		return err
	}
	entry <- record
	for record.Attempted < 1 {
		if err = waitPrivateQueueSettlement(ctx); err != nil {
			return err
		}
		if record, err = readPrivateQueueMaintenanceRecord(base); err != nil {
			return err
		}
	}
	return nil
}

// A custody fault keeps ordinary row cleanup, settles one failed attempt
// without spinning, and leaves full scope owed. The next accepted wake retries
// custody at full scope.
func TestScheduledPrivateMaintenanceCustodyFaultSettlesWithoutBlockingRemoval(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	custodyFault := errors.New("custody pool root is missing")
	recorder := &privateCustodyRecorder{}
	var failing atomic.Bool
	failing.Store(true)
	client.options.PrivateTerminalCustodyReleaser = func(_ context.Context, _ string, _ task.SupervisorRef, scope PrivateTerminalCustodyScope) error {
		recorder.record(scope, nil)
		if failing.Load() {
			return custodyFault
		}
		return nil
	}
	logOutput := captureMaintenanceProbeLog(t)
	record := runRequestedPrivateMaintenanceForTest(t, client, fixture.stateRoot, requestPrivateQueueMaintenance)
	requirePrivateRowRemoved(t, fixture.stateRoot, true)
	scopes, fault := recorder.snapshot()
	if record != (privateQueueMaintenanceRecord{SchemaVersion: task.SchemaVersion, Requested: 1, Attempted: 1, FullRequested: 1}) || len(scopes) != 2 || !scopes[0].Full || scopes[1].Full || fault != nil {
		t.Fatalf("failed custody state=%+v scopes=%+v, want one settled full attempt without completion", record, scopes)
	}
	if output := logOutput(); !strings.Contains(output, custodyFault.Error()) {
		t.Fatalf("custody fault log=%q, want the reported cause", output)
	}

	failing.Store(false)
	record = runRequestedPrivateMaintenanceForTest(t, client, fixture.stateRoot, requestPrivateCandidateQueueMaintenance)
	scopes, fault = recorder.snapshot()
	if record.Attempted != 2 || record.Completed != 2 || record.FullCompleted != 1 || len(scopes) != 4 || !scopes[2].Full || scopes[3].Full || fault != nil {
		t.Fatalf("retried custody state=%+v scopes=%+v, want the owed full scope completed", record, scopes)
	}
}

// Custody runs between protected phases, so removal must revalidate both the
// generation and the captured row identity after it.
func TestPrivateMaintenanceRevalidatesAfterCustody(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(t *testing.T, root string) error
		want   error
	}{
		{name: "reused numeric ID", change: replacePrivateRowLabel, want: ErrCleanupAuthorization},
		{name: "changed generation", change: func(_ *testing.T, root string) error { return markPrivateBootstrapPending(root) }, want: ErrPrivateSupervisorMigrationRequired},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, client := newMaintenanceProbeFixture(t)
			var authorizations atomic.Int32
			client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
				authorizations.Add(1)
				return privateTerminalTriggerCleanupAuthorizer(ctx, root, binding, jobs)
			}
			client.options.PrivateTerminalCustodyReleaser = func(_ context.Context, root string, _ task.SupervisorRef, scope PrivateTerminalCustodyScope) error {
				if scope.Full {
					return nil
				}
				return testCase.change(t, root)
			}
			logOutput := captureMaintenanceProbeLog(t)
			record := runRequestedPrivateMaintenanceForTest(t, client, fixture.stateRoot, requestPrivateQueueMaintenance)
			requirePrivateRowRemoved(t, fixture.stateRoot, false)
			if record.Attempted != 1 || record.Completed != 0 {
				t.Fatalf("refused revalidation state=%+v, want one settled failed attempt", record)
			}
			if errors.Is(testCase.want, ErrPrivateSupervisorMigrationRequired) && authorizations.Load() != 0 {
				t.Fatalf("changed generation reached authorization %d times", authorizations.Load())
			}
			if output := logOutput(); !strings.Contains(output, testCase.want.Error()) {
				t.Fatalf("refused revalidation log=%q, want %v", output, testCase.want)
			}
		})
	}
}

func replacePrivateRowLabel(t *testing.T, root string) error {
	row := statusTestJob(0, "delegate:reused:identity", statusStreamTestDoneState("Success"))
	data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	return os.WriteFile(filepath.Join(root, privateSupervisorDirectory, "test-status.json"), data, 0o600)
}

// A removal command that outlives its observer keeps the generation
// unattempted and the generation lease held until its exact Wait. The owner
// then resumes the same generation to completion.
func TestScheduledPrivateMaintenancePendingRemovalRetainsLeaseAfterCustody(t *testing.T) {
	owner := newPrivateStatusFixture(t)
	fixture, client := newMaintenanceProbeFixtureAtBase(t, owner)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	var custodyCalls atomic.Int32
	client.options.PrivateTerminalCustodyReleaser = func(context.Context, string, task.SupervisorRef, PrivateTerminalCustodyScope) error {
		custodyCalls.Add(1)
		return nil
	}
	started, release := filepath.Join(owner.base, "remove-started"), owner.releasePath
	client.options.ObservationTimeout = 100 * time.Millisecond
	client.options.Environment = append(os.Environ(), "PRIVATE_REMOVE_STARTED="+started, "PRIVATE_REMOVE_RELEASE="+release)
	if _, err := requestPrivateQueueMaintenance(t.Context(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	scope := newTestCallScope(t, client.options)
	scope.onRelease(func() { releasePrivateStatusOnCleanup(t, release) })
	call := startTestCall(scope, func() privateMaintenanceCallResult {
		return privateMaintenanceCallResult{available: client.RunRequestedPrivateQueueMaintenance(t.Context())}
	})
	waitForPrivateFixtureMarker(t, started)
	time.Sleep(3 * client.options.ObservationTimeout)
	supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
	if record := readPrivateMaintenanceRecordForTest(t, fixture.stateRoot); record.Attempted != 0 {
		t.Fatalf("pending removal consumed its generation: %+v", record)
	}
	releasePrivateStatus(t, release)
	select {
	case <-call.done:
		if !call.result.available {
			t.Fatal("private maintenance was not run")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("pending removal did not resume after its natural completion")
	}
	record := readPrivateMaintenanceRecordForTest(t, fixture.stateRoot)
	if record.Attempted != 1 || record.Completed != 1 || custodyCalls.Load() < 1 {
		t.Fatalf("resumed state=%+v custody calls=%d, want the same generation completed", record, custodyCalls.Load())
	}
	requirePrivateRowRemoved(t, fixture.stateRoot, true)
}

// The custody port receives its own copy of the captured rows. A consumer that
// rewrites a supplied label cannot change what authorization later sees or the
// identity that removal revalidates.
func TestPrivateMaintenanceCustodyScopeCannotAlterCapture(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	var authorizedLabel atomic.Value
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		if len(jobs) == 1 && jobs[0].Label != nil {
			authorizedLabel.Store(*jobs[0].Label)
		}
		return privateTerminalTriggerCleanupAuthorizer(ctx, root, binding, jobs)
	}
	client.options.PrivateTerminalCustodyReleaser = func(_ context.Context, _ string, _ task.SupervisorRef, scope PrivateTerminalCustodyScope) error {
		for _, job := range scope.Candidates {
			if job.Label != nil {
				*job.Label = "delegate:custody:rewritten"
			}
		}
		return nil
	}
	record := runRequestedPrivateMaintenanceForTest(t, client, fixture.stateRoot, requestPrivateQueueMaintenance)
	if got, stored := authorizedLabel.Load().(string); !stored || got != "finished" {
		t.Fatalf("authorizer saw label %q, want the captured label %q", got, "finished")
	}
	requirePrivateRowRemoved(t, fixture.stateRoot, true)
	if record.Attempted != 1 || record.Completed != 1 {
		t.Fatalf("maintenance state=%+v, want one completed pass", record)
	}
}

// Full recovery owes custody whose release needs no queue observation. With
// valid supervisor records and a stopped endpoint, the full-recovery phase
// still runs once, with no candidates, before the queue is observed. The failed
// capture then publishes no evidence, hands no candidates to custody, starts no
// daemon, and settles a failed attempt. This proves protocol order only; the
// application's canonical pool release is composed downstream.
func TestFullPrivateMaintenanceReleasesCustodyBeforeUnavailableQueue(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	client.options.SuccessfulCleanupAuthorizer = privateTerminalTriggerCleanupAuthorizer
	var preparations, statusEntries atomic.Int32
	client.options.PrivateTerminalEvidencePreparer = func(context.Context, string, task.SupervisorRef, []Job) error {
		preparations.Add(1)
		return nil
	}
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "status") {
			statusEntries.Add(1)
		}
	}
	recorder := &privateCustodyRecorder{}
	client.options.PrivateTerminalCustodyReleaser = func(_ context.Context, _ string, _ task.SupervisorRef, scope PrivateTerminalCustodyScope) error {
		var fault error
		if statusEntries.Load() != 0 {
			fault = errors.New("full-recovery custody ran after queue observation")
		}
		recorder.record(scope, fault)
		return nil
	}
	starts := readPrivateSupervisorFile(t, filepath.Join(base, "daemon-started"))
	if err := os.Remove(filepath.Join(base, "ready")); err != nil {
		t.Fatal(err)
	}
	logOutput := captureMaintenanceProbeLog(t)
	record := runRequestedPrivateMaintenanceForTest(t, client, fixture.stateRoot, requestPrivateQueueMaintenance)
	scopes, fault := recorder.snapshot()
	if len(scopes) != 1 || !scopes[0].Full || len(scopes[0].Candidates) != 0 || fault != nil {
		t.Fatalf("custody scopes=%+v fault=%v, want one candidate-free full phase before queue observation", scopes, fault)
	}
	if preparations.Load() != 0 || statusEntries.Load() == 0 {
		t.Fatalf("preparations=%d status entries=%d, want a failed capture with no published evidence", preparations.Load(), statusEntries.Load())
	}
	if record != (privateQueueMaintenanceRecord{SchemaVersion: task.SchemaVersion, Requested: 1, Attempted: 1, FullRequested: 1}) {
		t.Fatalf("unavailable queue state=%+v, want one settled failed full attempt", record)
	}
	requirePrivateRowRemoved(t, fixture.stateRoot, false)
	if after := readPrivateSupervisorFile(t, filepath.Join(base, "daemon-started")); string(after) != string(starts) {
		t.Fatalf("maintenance started the stopped daemon: before=%q after=%q", starts, after)
	}
	if logOutput() == "" {
		t.Fatal("unavailable queue failure was not reported")
	}
}
