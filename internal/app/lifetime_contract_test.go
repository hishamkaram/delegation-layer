package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type lifetimeContractPostLinkFault struct {
	taskdir.BaseFaultInjector

	mu     sync.Mutex
	target string
	fired  bool
}

func (f *lifetimeContractPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) != f.target {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired {
		return nil
	}
	f.fired = true
	return errors.New("injected post-link lifetime worker failure")
}

func (f *lifetimeContractPostLinkFault) didFire() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

func newProductionRecordGuardianFixture(
	t *testing.T,
	rootID string,
	taskID string,
	ownerKind string,
	deadline time.Time,
	records lifetimeGuardianRecords,
	acquireOwner func(context.Context) (lifetimeGuardianOwner, error),
	configure func(*lifetimeGuardianOptions),
) *lifetimeGuardianTestFixture {
	t.Helper()
	return newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *lifetimeGuardianOptions) {
			options.rootID = rootID
			options.taskID = taskID
			options.ownerKind = ownerKind
			options.deadline = deadline
			options.now = time.Now
			options.records = records
			options.acquireOwner = acquireOwner
			options.waitRecordRetry = func(context.Context) error { return nil }
			options.finalizationBackoff = func(context.Context, time.Duration) error { return nil }
			if configure != nil {
				configure(options)
			}
		},
	)
}

func callProductionRecordGuardian(t *testing.T, fixture *lifetimeGuardianTestFixture, taskID, kind string) {
	t.Helper()
	fixture.callMust(t, kind, lifetimeMessage{
		Version: lifetimeProtocolVersion,
		Type:    kind,
		TaskID:  taskID,
	})
}

func TestLifetimeGuardianTaskStoreAmbiguousWorkerWriteReachesStopConsumer(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, request := newAppTaskInStore(t, store, lifetimeGuardianTestTaskID, false, nil)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })

	fault := &lifetimeContractPostLinkFault{target: "worker.json"}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	deadline := time.Now().Add(time.Minute).UTC()
	fixture := newProductionRecordGuardianFixture(
		t,
		request.RootID,
		request.TaskID,
		task.LifetimeOwnerTask,
		deadline,
		td,
		func(ctx context.Context) (lifetimeGuardianOwner, error) {
			return td.AcquireLifetimeOwner(ctx)
		},
		nil,
	)

	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageHello)
	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageClose)
	fixture.worker.release()
	if runErr := fixture.waitRun(t); runErr != nil {
		t.Fatalf("guardian with production task store: %v", runErr)
	}
	if !fault.didFire() {
		t.Fatal("production task store did not exercise the ambiguous worker write")
	}
	worker, err := td.ReadLifetimeWorker()
	if err != nil {
		t.Fatal(err)
	}
	wait, err := td.ReadLifetimeWorkerWait()
	if err != nil {
		t.Fatal(err)
	}
	if worker.PID != 303 || wait.ChildPID != worker.PID || wait.WorkerSHA256 == "" ||
		wait.Result != task.LifetimeWorkerWaitSuccess {
		t.Fatalf("production task lifetime proof = worker %+v wait %+v", worker, wait)
	}
	ready, err := td.StopTerminationReady()
	if err != nil || !ready {
		t.Fatalf("task stop consumer = ready %t, err %v", ready, err)
	}
}

func TestLifetimeGuardianInspectionStoreAmbiguousWorkerWriteReachesAdmissionConsumer(t *testing.T) {
	created := time.Now().UTC()
	store, operation := newInspectionWorkerOperationWithExitProof(
		t,
		created,
		task.RunnerOwnershipManaged,
		inspection.WorkerExitProofGuardianWaitV1,
	)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}

	fault := &lifetimeContractPostLinkFault{target: "lifetime-worker.json"}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	request := operation.Request()
	fixture := newProductionRecordGuardianFixture(
		t,
		request.RootID,
		request.TaskID,
		task.LifetimeOwnerInspection,
		operation.Deadline(),
		operation,
		func(ctx context.Context) (lifetimeGuardianOwner, error) {
			return operation.AcquireLifetimeOwner(ctx)
		},
		nil,
	)
	waitErr := errors.New("injected managed inspection worker failure")
	fixture.worker.setWaitError(waitErr)

	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageHello)
	callProductionRecordGuardian(t, fixture, request.TaskID, lifetimeMessageClose)
	fixture.worker.release()
	if runErr := fixture.waitRun(t); !errors.Is(runErr, waitErr) {
		t.Fatalf("guardian failure = %v, want worker failure", runErr)
	}
	if !fault.didFire() {
		t.Fatal("production inspection store did not exercise the ambiguous worker write")
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || result != nil || succeeded {
		t.Fatalf("inspection consumer = result %#v, succeeded %t, err %v", result, succeeded, err)
	}
	evaluation, err := evaluateInspectionAdmission(t.Context(), operation)
	if err != nil || evaluation.decision != inspectionAdmissionUnavailable || !evaluation.recorded {
		t.Fatalf("admission consumer = %#v, err %v", evaluation, err)
	}
}
