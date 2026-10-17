package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRunInspectionWorkerRejectsInvalidTaskBeforeOpeningState(t *testing.T) {
	called := false
	got := runInspectionWorker(filepath.Join(testsupport.CanonicalTempDir(t), "missing"), "invalid", Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			called = true
			return commonprovider.ProfileCandidate{}, nil
		},
	})
	if !errors.Is(got, admissioncore.ErrInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	if called {
		t.Fatal("candidate preparation ran for an invalid task ID")
	}
}

func TestRunInspectionWorkerNeverCreatesMissingStore(t *testing.T) {
	root := filepath.Join(testsupport.CanonicalTempDir(t), "missing-state")
	got := runInspectionWorker(root, strings.Repeat("a", 32), Dependencies{})
	if !errors.Is(got, admissioncore.ErrInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker created or changed missing store %q: %v", root, err)
	}
}

func TestInspectionWorkerFailureExposesOnlySafeStage(t *testing.T) {
	err := inspectionWorkerFailure("prepare-environment")
	if !errors.Is(err, admissioncore.ErrInspectionWorkerUnavailable) {
		t.Fatalf("staged failure lost worker classification: %v", err)
	}
	public := inspectionWorkerPublicError(err)
	if got, want := public.Error(), "native inspection unavailable: stage=prepare-environment"; got != want {
		t.Fatalf("public failure = %q, want %q", got, want)
	}
	if got := inspectionWorkerPublicError(errors.New("provider secret detail")).Error(); got != admissioncore.ErrInspectionWorkerUnavailable.Error() {
		t.Fatalf("unclassified failure leaked detail: %q", got)
	}
}

func TestValidateInspectionWorkerExecutableRejectsWrongExecutable(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	binding := operation.Request().Binding
	if err := validateInspectionWorkerExecutable(context.Background(), testsupport.CanonicalTempDir(t), operation, binding, filepath.Join(testsupport.CanonicalTempDir(t), "current-worker")); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("wrong worker executable was accepted: %v", err)
	}
}

func TestValidateInspectionWorkerExecutableRequiresStateRunnerForUpgrade(t *testing.T) {
	t.Run("authorized content-addressed state runner", func(t *testing.T) {
		store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), task.RunnerOwnershipManaged)
		source := canonicalAppTestExecutable(t)
		runner := installStateRunnerFixture(t, store.Root, source)
		if err := operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := operation.Request().Binding
		if err := validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); err != nil {
			t.Fatalf("verified state-root worker upgrade was rejected: %v", err)
		}
	})

	t.Run("authorized install runner", func(t *testing.T) {
		store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), task.RunnerOwnershipManaged)
		runner := canonicalAppTestExecutable(t)
		if err := operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := operation.Request().Binding
		if err := validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("authorized install runner was accepted for task upgrade: %v", err)
		}
	})
}

func TestValidateInspectionWorkerExecutableRejectsUnmanagedUpgradeMarker(t *testing.T) {
	for _, ownership := range []string{"", task.RunnerOwnershipCustom} {
		name := map[string]string{"": "unknown", task.RunnerOwnershipCustom: "custom"}[ownership]
		t.Run(name, func(t *testing.T) {
			store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), ownership)
			source := canonicalAppTestExecutable(t)
			runner := installStateRunnerFixture(t, store.Root, source)
			control, err := store.OpenInspection(operation.Request().TaskID, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = control.Put("worker-upgrade.json", inspection.ManagedWorkerUpgradeRecord{
				SchemaVersion: task.SchemaVersion,
				RequestSHA256: operation.Digest(),
			})
			err = errors.Join(err, control.Close())
			if err != nil {
				t.Fatal(err)
			}
			binding := operation.Request().Binding
			if err = validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("state-root successor accepted unmanaged ownership %q: %v", ownership, err)
			}
		})
	}
}

func TestInspectionBudgetStopperRejectsClosedOperation(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	stopper := &inspectionBudgetStopper{operation: operation, client: &pueue.Client{}}
	if request, err := stopper.PrepareBudget(operation.Deadline()); request != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("closed operation yielded stop authority: request=%v err=%v", request, err)
	}
}

type inspectionLifetimeStub struct {
	terminated bool
}

func (*inspectionLifetimeStub) StartCommand(*exec.Cmd, func() error) error { return nil }
func (*inspectionLifetimeStub) FinishCommand(context.Context, *exec.Cmd) error {
	return nil
}

func (s *inspectionLifetimeStub) Terminate(context.Context) error {
	s.terminated = true
	return nil
}
func (*inspectionLifetimeStub) CloseTask(context.Context) error { return nil }

func TestInspectionBudgetStopperUsesGuardianWithoutClaimingQueueEnded(t *testing.T) {
	store, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	lifetime := &inspectionLifetimeStub{}
	stopper := &inspectionBudgetStopper{operation: operation, client: &pueue.Client{}, lifetime: lifetime}
	request, err := stopper.PrepareBudget(operation.Deadline())
	if err != nil {
		t.Fatal(err)
	}
	if err = request(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !lifetime.terminated {
		t.Fatal("inspection deadline did not use the guardian lifetime")
	}
	var reply inspection.StopReplyRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-reply.json", &reply)
	if reply.NumericTaskID != 42 || reply.Action != "terminate" || !reply.Acknowledged {
		t.Fatalf("unexpected lifetime stop reply: %+v", reply)
	}
	control, err := store.OpenInspection(operation.Request().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var observation inspection.StopObservationRecord
	if err = control.Read("stop-observation.json", &observation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian acknowledgment fabricated queue-ended observation: %v", err)
	}
}

type inspectionExpiryTimer struct {
	ch chan time.Time
}

func (t *inspectionExpiryTimer) C() <-chan time.Time { return t.ch }
func (*inspectionExpiryTimer) Stop() bool            { return true }

type inspectionExpiryClock struct {
	mu    sync.Mutex
	now   time.Time
	timer *inspectionExpiryTimer
}

func (c *inspectionExpiryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *inspectionExpiryClock) NewTimer(time.Duration) execution.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = &inspectionExpiryTimer{ch: make(chan time.Time, 1)}
	return c.timer
}

func (c *inspectionExpiryClock) expire(deadline time.Time) {
	c.mu.Lock()
	c.now = deadline
	timer := c.timer
	c.mu.Unlock()
	timer.ch <- deadline
}

type inspectionExpiryLifetime struct {
	terminated chan struct{}
	closed     chan struct{}
}

func (*inspectionExpiryLifetime) StartCommand(*exec.Cmd, func() error) error { return nil }
func (*inspectionExpiryLifetime) FinishCommand(context.Context, *exec.Cmd) error {
	return nil
}

func (l *inspectionExpiryLifetime) Terminate(context.Context) error {
	close(l.terminated)
	return nil
}

func (l *inspectionExpiryLifetime) CloseTask(context.Context) error {
	close(l.closed)
	return nil
}

func TestInspectionExpiryPersistsTerminationReplyBeforeLifetimeClose(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	store, operation := newInspectionWorkerOperation(t, created)
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	clock := &inspectionExpiryClock{now: created}
	lifetime := &inspectionExpiryLifetime{terminated: make(chan struct{}), closed: make(chan struct{})}
	stopper := &inspectionBudgetStopper{operation: operation, client: &pueue.Client{}, lifetime: lifetime}
	entered := make(chan struct{})
	type result struct{ workErr, stopErr error }
	done := make(chan result, 1)
	go func() {
		workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{
			Clock: clock, Stopper: stopper, Lifetime: lifetime,
		}, func(scope execution.PreflightScope) error {
			close(entered)
			<-scope.Context().Done()
			return nil
		})
		done <- result{workErr: workErr, stopErr: stopErr}
	}()
	<-entered
	clock.expire(operation.Deadline())
	<-lifetime.terminated
	got := <-done
	if got.workErr == nil || got.stopErr != nil {
		t.Fatalf("expiry result work=%v stop=%v", got.workErr, got.stopErr)
	}
	select {
	case <-lifetime.closed:
	default:
		t.Fatal("supervised expiry returned before lifetime close")
	}
	var reply inspection.StopReplyRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-reply.json", &reply)
	if reply.NumericTaskID != 42 || reply.Action != "terminate" || !reply.Acknowledged {
		t.Fatalf("expiry did not persist guardian termination acknowledgment: %+v", reply)
	}
}

func TestCompleteInspectionFailureSealsExpiredResult(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	permit, err := operation.ClaimStart(time.Unix(101, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if got := completeInspectionFailure(operation); !errors.Is(got, admissioncore.ErrInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	result, _, err := operation.ReadCompleted()
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != inspection.ResultExpired || len(result.Facts) != 0 {
		t.Fatalf("expired result=%+v", result)
	}
}

func TestRecordInspectionStopSeparatesAcknowledgmentFromEndedObservation(t *testing.T) {
	store, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	if _, _, err := operation.ClaimStop(operation.Deadline()); err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	acknowledged := true
	if err := recordInspectionStop(context.Background(), operation, pueue.StopResult{NumericTaskID: &id, Action: "kill", Acknowledged: &acknowledged, ObservedState: pueue.StateRunning}); err != nil {
		t.Fatal(err)
	}
	var reply inspection.StopReplyRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-reply.json", &reply)
	if reply.NumericTaskID != id || reply.Action != "kill" || !reply.Acknowledged {
		t.Fatalf("unexpected stop reply: %+v", reply)
	}
	if err := recordInspectionStop(context.Background(), operation, pueue.StopResult{NumericTaskID: &id, ObservedState: pueue.StateEnded}); err != nil {
		t.Fatal(err)
	}
	var observation inspection.StopObservationRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-observation.json", &observation)
	if observation.NumericTaskID != id || observation.State != "ended" {
		t.Fatalf("unexpected stop observation: %+v", observation)
	}
}

func readInspectionWorkerRecord(t *testing.T, store *taskdir.Store, taskID, name string, record any) {
	t.Helper()
	control, err := store.OpenInspection(taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = control.Read(name, record); err != nil {
		t.Fatal(err)
	}
}

func newInspectionWorkerOperation(t *testing.T, created time.Time) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	return admissionfixture.NewInspectionWorkerOperation(t, created)
}

func newInspectionWorkerOperationWithRunnerOwnership(t *testing.T, created time.Time, runnerOwnership string) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	return admissionfixture.NewInspectionWorkerOperationWithRunnerOwnership(t, created, runnerOwnership)
}

func newInspectionWorkerOperationWithExitProof(t *testing.T, created time.Time, runnerOwnership, workerExitProof string) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	return admissionfixture.NewInspectionWorkerOperationWithExitProof(t, created, runnerOwnership, workerExitProof)
}
