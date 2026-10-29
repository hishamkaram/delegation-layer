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
	"testing/synctest"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// placedTerminalInspection is one ended inspection row on a published private
// supervisor whose journal records a closed lifetime with a bound pool view.
type placedTerminalInspection struct {
	store        *taskdir.Store
	operation    *inspection.Operation
	job          pueue.Job
	registration *task.LifetimeRegistrationRecord
}

func newPlacedTerminalInspection(t *testing.T) placedTerminalInspection {
	t.Helper()
	store := newBareInspectionStore(t)
	supervisor, _, _ := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	registration := recordCustodyLifetimeUntil(t, operation, newCustodyTestPlacement(t), task.LifetimeTerminationCompleted, operation.Deadline())
	bindCustodyTestView(t, registration)
	writeTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(true))
	return placedTerminalInspection{store: store, operation: operation, job: job, registration: registration}
}

func (f placedTerminalInspection) requireRowRetained(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(f.store.Root, ".supervisor", "terminal-event-removals")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an inspection row with unreleased custody was removed: %v", err)
	}
	if !custodyTestPending(t, f.registration) {
		t.Fatal("a missing pool was treated as released")
	}
}

func (f placedTerminalInspection) requireReleasedAndRemoved(t *testing.T) {
	t.Helper()
	waitForTestFileContent(t, filepath.Join(f.store.Root, ".supervisor", "terminal-event-removals"), "7\n")
	if custodyTestPending(t, f.registration) {
		t.Fatal("the elected owner removed the row without releasing its recorded pool")
	}
}

// TestElectedOwnerReleasesInspectionCustodyBeforeSamePassRemoval drives the
// production terminal callback and elected owner against a published private
// supervisor. A missing recorded pool fails the custody phase and the real
// row authorizer keeps the row; once the pool is back, the same pass releases
// the exact recorded pool and that release authorizes the row's removal.
func TestElectedOwnerReleasesInspectionCustodyBeforeSamePassRemoval(t *testing.T) {
	f := newPlacedTerminalInspection(t)
	restore := moveCustodyTestPool(t, f.registration.CustodyPlacement)
	runTerminalEventMaintenanceForTest(t, f.store, f.job.ID, nil, nil)
	restore()
	f.requireRowRetained(t)
	runTerminalEventMaintenanceForTest(t, f.store, f.job.ID, nil, nil)
	f.requireReleasedAndRemoved(t)
}

// composedWatchdog bounds one step that must not wait for the blocked custody
// phase. It only escapes a hang: firing is a test failure, never ordering, and
// never permission to abandon work that is still running.
const composedWatchdog = 30 * time.Second

var errComposedWatchdog = errors.New("composed test watchdog fired")

// blockedCustodyOwner owns the lifetime of the elected owner held at its
// custody barrier and of every step run while it is held. The barrier opens
// once; close joins the owner, and every started step is joined before its
// result is returned, so no fixture teardown can run while either is active.
type blockedCustodyOwner struct {
	entered  chan struct{}
	gate     chan struct{}
	release  func()
	finished chan struct{}
	err      error
	close    func() error
}

func newBlockedCustodyOwner() *blockedCustodyOwner {
	owner := &blockedCustodyOwner{entered: make(chan struct{}), gate: make(chan struct{}), finished: make(chan struct{})}
	owner.release = sync.OnceFunc(func() { close(owner.gate) })
	owner.close = sync.OnceValue(owner.join)
	return owner
}

// start runs the owner body; the caller registers close before it starts.
func (o *blockedCustodyOwner) start(run func() error) {
	go func() {
		o.err = run()
		close(o.finished)
	}()
}

// join opens the barrier and waits for the owner's own return. A watchdog
// fault is reported only after the owner has finished.
func (o *blockedCustodyOwner) join() error {
	o.release()
	select {
	case <-o.finished:
		return o.err
	case <-time.After(composedWatchdog):
		<-o.finished
		return errors.Join(o.err, fmt.Errorf("%w: elected owner outlived its opened custody barrier", errComposedWatchdog))
	}
}

// runStep runs one step that must answer while custody is held. When the
// watchdog fires it opens the barrier so the step can finish, and still joins
// the step before returning its own error beside the watchdog fault.
func (o *blockedCustodyOwner) runStep(step string, run func() error) error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(composedWatchdog):
		o.release()
		return errors.Join(<-done, fmt.Errorf("%w: %s waited for the blocked custody phase", errComposedWatchdog, step))
	}
}

func requireReturnsWhileBlocked(t *testing.T, owner *blockedCustodyOwner, step string, run func() error) {
	t.Helper()
	if err := owner.runStep(step, run); err != nil {
		t.Fatalf("%s while custody was blocked: %v", step, err)
	}
}

// startBlockedCustodyOwner runs the real elected owner with the canonical
// custody releaser wrapped by the owner's barrier. The saved releaser runs
// unchanged once the barrier opens.
func startBlockedCustodyOwner(t *testing.T, root string) *blockedCustodyOwner {
	t.Helper()
	deps := terminalEventMaintenanceDependencies(nil, nil)
	canonical := deps.SupervisorOptions.PrivateTerminalCustodyReleaser
	owner := newBlockedCustodyOwner()
	var enter sync.Once
	deps.SupervisorOptions.PrivateTerminalCustodyReleaser = func(ctx context.Context, stateRoot string, supervisor task.SupervisorRef, scope pueue.PrivateTerminalCustodyScope) error {
		if scope.Full {
			enter.Do(func() { close(owner.entered) })
			<-owner.gate
		}
		return canonical(ctx, stateRoot, supervisor, scope)
	}
	if err := pueue.RequestPrivateQueueMaintenance(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.close(); err != nil {
			t.Error(err)
		}
	})
	owner.start(func() error {
		return supervisorcore.RunQueueMaintenance(root, deps.SupervisorOptions, deps.supervisorLookup())
	})
	select {
	case <-owner.entered:
	case <-time.After(composedWatchdog):
		t.Fatal("the elected owner never entered its full custody phase")
	}
	return owner
}

// TestBlockedCustodyPhaseHoldsNoApplicationLease: while the real elected
// owner's canonical custody release of a closed ordinary task is held inside
// the custody phase, public status and collect answer, bootstrap EX is
// granted, and the public path neither reclaims nor removes anything. Opening
// the barrier lets the owner release the exact recorded pool. This proves the
// application holds no lease across a blocked custody operation; it does not
// reproduce a kernel filesystem stall.
func TestBlockedCustodyPhaseHoldsNoApplicationLease(t *testing.T) {
	store := newBareInspectionStore(t)
	var enqueues atomic.Int32
	supervisor, _, _ := admissionfixture.NewTerminalEventInspectionSupervisorWithObserver(t, store, admissioncore.TerminalEvidence{}, nil, func(event pueue.CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "enqueue") && !slices.Contains(event.Argv, "--help") {
			enqueues.Add(1)
		}
	})
	taskID := strings.Repeat("d", 32)
	_, registration := newPlacedTaskInStore(t, store, taskID, pueue.PrivateConfigPath(store.Root), task.LifetimeTerminationCompleted)
	staged, meta := newStagedSubmissionTask(t, store, supervisor.Binding())
	if err := supervisorcore.EnsureAdmissionSchedulingContext(t.Context(), store.Root, supervisor); err != nil {
		t.Fatal(err)
	}
	owner := startBlockedCustodyOwner(t, store.Root)

	// A staged ordinary submission crosses the queue identity boundary: add,
	// durable receipt commit, then enqueue of that exact row.
	requireReturnsWhileBlocked(t, owner, "staged submission", func() error {
		return requireStagedSubmission(staged, meta, supervisor, &enqueues)
	})

	// A closed lifetime without a seal is an interruption: status observes it
	// successfully and collect reports the canonical interruption failure.
	requireReturnsWhileBlocked(t, owner, "status", func() error {
		result := custodyObservationCommands()["status"](store, taskID)
		if result.code != 0 || result.err != nil || result.response.TaskID != taskID {
			return fmt.Errorf("status = code %d task=%q: %w", result.code, result.response.TaskID, result.err)
		}
		return nil
	})
	requireReturnsWhileBlocked(t, owner, "collect", func() error {
		result := custodyObservationCommands()["collect"](store, taskID)
		if result.code != 1 || !errors.Is(result.err, errTaskInterrupted) {
			return fmt.Errorf("collect = code %d: %w", result.code, result.err)
		}
		return nil
	})
	requireReturnsWhileBlocked(t, owner, "bootstrap EX", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), composedWatchdog)
		defer cancel()
		return pueue.WithBootstrapLockContext(ctx, store.Root, func(context.Context) error { return nil })
	})
	if !custodyTestPending(t, registration) {
		t.Fatal("a public observer reclaimed custody while the owner held it")
	}

	if err := owner.close(); err != nil {
		t.Fatalf("elected owner: %v", err)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("the elected owner did not release the exact recorded pool")
	}
}

// withCanonicalCleanupCallbacks replaces the historical fixture's empty-queue
// stubs with the production application callbacks.
func withCanonicalCleanupCallbacks(options pueue.Options) pueue.Options {
	options.SuccessfulCleanupAuthorizer = authorizePrivateSuccessfulCleanup
	options.PrivateTerminalEvidencePreparer = preparePrivateTerminalEvidence
	options.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	options.PrivateTerminalCustodyReleaser = releasePrivateTerminalCustody
	return options
}

func historicalCommandCount(t *testing.T, fixture *historicalSupervisorFixture, command string) int {
	t.Helper()
	return strings.Count(string(readHistoricalSupervisorBytes(t, fixture.commandLog)), " "+command+"\n")
}

// TestGuardianCustodyFaultSurvivesFullMigrationUntilFullMaintenance composes
// the guardian, a full explicit supervisor migration and the elected owner on
// one historical root. The guardian's release fault on the predecessor's
// ended inspection persists a full wake. Migration succeeds while the pool is
// unavailable, completes the successor and the inspection edge, preserves the
// row and rewrites no inspection record. With the same pool restored, the
// successor's full maintenance releases the exact recorded pool and
// dispatches the authorized removal. The fixture client only records the
// remove command; it does not delete the row from its status file.
func TestGuardianCustodyFaultSurvivesFullMigrationUntilFullMaintenance(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	fixture.options = withCanonicalCleanupCallbacks(fixture.options)
	operation, job := newTerminalInspectionCleanupOperationForTask(t, fixture.store, fixture.predecessorClient, strings.Repeat("b", 32), 7)
	registration := recordCustodyLifetimeUntil(t, operation, newCustodyTestPlacement(t), task.LifetimeTerminationCompleted, operation.Deadline())
	bindCustodyTestView(t, registration)
	writeHistoricalInspectionStatus(t, fixture, job)
	records := snapshotRebootTaskFiles(t, filepath.Join(fixture.root, "inspections"))

	before := readTaskMaintenanceWake(t, fixture.root)
	restore := moveCustodyTestPool(t, registration.CustodyPlacement)
	releaseGuardianLifetimeCustody(fixture.root, operation, true)
	requireTaskMaintenanceFullWake(t, fixture.root, before)
	resetHistoricalSupervisorCommandLog(t, fixture)
	result := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
	if result.code != 0 || result.err != nil {
		t.Fatalf("full migration with an unavailable custody pool = code %d err=%v", result.code, result.err)
	}
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || task.SameSupervisorIdentity(active, fixture.predecessor) {
		t.Fatalf("migration did not publish a successor: %+v found=%t err=%v", active, found, err)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, active); completeErr != nil || !complete {
		t.Fatalf("successor history = %t %v", complete, completeErr)
	}
	successorGroup := scenario.legacyGroup
	successorGroup.Supervisor = active
	if complete, edgeErr := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, successorGroup); edgeErr != nil || !complete {
		t.Fatalf("inspection migration edge = %t %v", complete, edgeErr)
	}
	if removes := historicalCommandCount(t, fixture, "remove"); removes != 0 {
		t.Fatalf("migration dispatched %d removals of the preserved row", removes)
	}
	requireRebootTaskFilesUnchanged(t, records)
	restore()
	if !custodyTestPending(t, registration) {
		t.Fatal("migration released custody from an unavailable pool")
	}

	deps := fixture.dependencies().normalized()
	if err = supervisorcore.RunQueueMaintenance(fixture.root, deps.SupervisorOptions, deps.supervisorLookup()); err != nil {
		t.Fatal(err)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("full maintenance did not release the exact recorded pool")
	}
	if removes := historicalCommandCount(t, fixture, "remove"); removes != 1 {
		t.Fatalf("full maintenance dispatched %d removals, want the one authorized row", removes)
	}
}

func newTerminalInspectionCleanupOperationForTask(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, taskID string, jobID int64) (*inspection.Operation, pueue.Job) {
	t.Helper()
	return admissionfixture.NewTerminalInspectionCleanupOperationForTask(t, store, supervisor, inspection.ResultEligible, true, taskID, jobID)
}

// writeHistoricalInspectionStatus publishes the ended inspection row in the
// status both fixture generations report.
func writeHistoricalInspectionStatus(t *testing.T, fixture *historicalSupervisorFixture, job pueue.Job) {
	t.Helper()
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(job.ID, 10): map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": admissionfixture.CleanupTestTerminalState(true),
		}},
		"groups": map[string]any{
			"default": map[string]any{"status": "Running", "parallel_tasks": 0},
			job.Group: map[string]any{"status": "Running", "parallel_tasks": 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, fixture.statusPath, status)
}

// newStagedSubmissionTask creates an unadmitted ordinary task whose immutable
// supervisor binding is the fixture's published private supervisor.
func newStagedSubmissionTask(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) (*taskdir.TaskDir, *task.MetaRecord) {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("staged submission during blocked custody")
	request := newHistoricalSupervisorTaskRecord(store.RootID, workspace, brief)
	profile := appTestExecutionProfile(&request, "/usr/bin/true", nil)
	meta := tasklifecycle.NewMeta(request, profile, binding, "/usr/bin/true", task.RunnerOwnershipCustom, "app-test")
	td, err := store.CreateTask(request.TaskID, &request, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	_, persisted, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	return td, persisted
}

// requireStagedSubmission submits through the canonical staged path and
// requires the matched admission, its durable receipt and the enqueue
// dispatch. The fixture transport answers add with row 9 and accepts enqueue;
// no real queue schedules or launches anything.
func requireStagedSubmission(td *taskdir.TaskDir, meta *task.MetaRecord, supervisor *pueue.Client, enqueues *atomic.Int32) error {
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		return err
	}
	result := tasklifecycle.SubmitWithClient(td, nil, meta, supervisor, "/usr/bin/true", permit)
	if result.Failure != nil {
		return result.Failure
	}
	if result.Admission != task.AdmissionAdmitted || result.Pending != "" {
		return fmt.Errorf("staged submission admission=%s pending=%q", result.Admission, result.Pending)
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return err
	}
	if receipt.NumericTaskID != 9 || !task.SameSupervisorIdentity(receipt.SupervisorRef(), supervisor.Binding()) {
		return fmt.Errorf("staged receipt = %+v", receipt)
	}
	if dispatched := enqueues.Load(); dispatched != 1 {
		return fmt.Errorf("staged submission dispatched %d enqueues, want 1", dispatched)
	}
	return nil
}

// TestBlockedCustodyOwnerJoinsWorkBeforeTeardown holds a step and the owner
// past the watchdog in virtual time. The watchdog fault is reported beside the
// work's own error only after that work has finished, so the stand-in for
// fixture teardown never observes live work. The test's own abort channel,
// registered first and so closed last, frees every goroutine even if the owner
// under test failed to.
func TestBlockedCustodyOwnerJoinsWorkBeforeTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		abort := make(chan struct{})
		t.Cleanup(func() { close(abort) })
		stepDone, ownerDone := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			if !barrierOpen(stepDone) || !barrierOpen(ownerDone) {
				t.Error("work was still active at fixture teardown")
			}
		})
		owner := newBlockedCustodyOwner()
		t.Cleanup(func() {
			if err := owner.close(); !errors.Is(err, errComposedWatchdog) {
				t.Errorf("owner close = %v, want its watchdog fault", err)
			}
		})
		ownerErr, stepErr := errors.New("owner result"), errors.New("step result")
		owner.start(func() error {
			// Held well past the close watchdog, which starts after the step.
			awaitBarrier(owner.gate, abort)
			awaitBarrier(virtualHold(3*composedWatchdog, abort), abort)
			close(ownerDone)
			return ownerErr
		})

		stepHold := virtualHold(composedWatchdog+time.Second, abort)
		err := owner.runStep("held step", func() error {
			awaitBarrier(owner.gate, abort)
			awaitBarrier(stepHold, abort)
			close(stepDone)
			return stepErr
		})
		if !barrierOpen(stepDone) {
			t.Fatal("the step result returned before the step finished")
		}
		if !errors.Is(err, stepErr) || !errors.Is(err, errComposedWatchdog) {
			t.Fatalf("step = %v, want its own error beside the watchdog fault", err)
		}

		err = owner.close()
		if !barrierOpen(ownerDone) {
			t.Fatal("owner close returned before the owner finished")
		}
		if !errors.Is(err, ownerErr) || !errors.Is(err, errComposedWatchdog) {
			t.Fatalf("owner close = %v, want its own error beside the watchdog fault", err)
		}
		if again := owner.close(); again == nil || again.Error() != err.Error() {
			t.Fatalf("second close = %v, want the same result", again)
		}
	})
}

// virtualHold returns a barrier that opens after delay, or on abort. Inside a
// synctest bubble the delay is virtual.
func virtualHold(delay time.Duration, abort <-chan struct{}) chan struct{} {
	hold := make(chan struct{})
	go func() {
		select {
		case <-time.After(delay):
		case <-abort:
		}
		close(hold)
	}()
	return hold
}

func awaitBarrier(barrier, abort <-chan struct{}) {
	select {
	case <-barrier:
	case <-abort:
	}
}

func barrierOpen(barrier <-chan struct{}) bool {
	select {
	case <-barrier:
		return true
	default:
		return false
	}
}
