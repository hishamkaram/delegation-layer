package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
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

func TestLifetimeGuardianTaskStoreAmbiguousWorkerWriteReachesStopConsumer(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(testsupportcore.CanonicalTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, request := newAppTaskInStore(t, store, lifetimeGuardianTestTaskID, false, nil)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })

	fault := &lifetimeContractPostLinkFault{target: "worker.json"}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	deadline := time.Now().Add(time.Minute).UTC()
	fixture := testsupport.NewRecordFixture(
		t,
		request.RootID,
		request.TaskID,
		task.LifetimeOwnerTask,
		deadline,
		td,
		func(ctx context.Context) (guardian.Owner, error) {
			return td.AcquireLifetimeOwner(ctx)
		},
		nil,
	)

	fixture.Hello(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if runErr := fixture.WaitRun(t); runErr != nil {
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
	fixture := testsupport.NewRecordFixture(
		t,
		request.RootID,
		request.TaskID,
		task.LifetimeOwnerInspection,
		operation.Deadline(),
		operation,
		func(ctx context.Context) (guardian.Owner, error) {
			return operation.AcquireLifetimeOwner(ctx)
		},
		nil,
	)
	waitErr := errors.New("injected managed inspection worker failure")
	fixture.SetWorkerWaitError(waitErr)

	fixture.Hello(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if runErr := fixture.WaitRun(t); !errors.Is(runErr, waitErr) {
		t.Fatalf("guardian failure = %v, want worker failure", runErr)
	}
	if !fault.didFire() {
		t.Fatal("production inspection store did not exercise the ambiguous worker write")
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || result != nil || succeeded {
		t.Fatalf("inspection consumer = result %#v, succeeded %t, err %v", result, succeeded, err)
	}
	facts, recorded, terminal, err := admissioncore.RecordedInspectionFacts(t.Context(), operation)
	if !errors.Is(err, admissioncore.ErrInspectionWorkerUnavailable) || !recorded || !terminal || facts != nil {
		t.Fatalf("admission consumer = facts %s, recorded %t, terminal %t, err %v", facts, recorded, terminal, err)
	}
}
