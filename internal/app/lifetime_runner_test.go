package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type lifetimeTestCloser interface {
	Close() error
}

func cleanupLifetimeTestResource(t *testing.T, resource lifetimeTestCloser) {
	t.Helper()
	t.Cleanup(func() {
		if err := resource.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close test resource: %v", err)
		}
	})
}

func TestLifetimeWorkerArgumentsPreserveOperationKind(t *testing.T) {
	taskID := strings.Repeat("a", 32)
	root := "/state/root"
	ordinary := lifetimeWorkerArguments(runnerArguments{Root: root, TaskID: taskID})
	if got, want := strings.Join(ordinary, " "), "--lifetime-child --lifetime-fd 3 --supervised --root "+root+" "+taskID; got != want {
		t.Fatalf("ordinary arguments = %q, want %q", got, want)
	}
	inspection := lifetimeWorkerArguments(runnerArguments{Root: root, TaskID: taskID, Inspection: true})
	if got, want := strings.Join(inspection, " "), "--inspection --lifetime-child --lifetime-fd 3 --supervised --root "+root+" "+taskID; got != want {
		t.Fatalf("inspection arguments = %q, want %q", got, want)
	}
}

func TestLifetimeWorkerCommandUsesCanonicalRootAndExplicitDirectory(t *testing.T) {
	realRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := tasklifecycle.ResolveRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	parsed := runnerArguments{Root: alias, TaskID: strings.Repeat("b", 32)}
	worker, err := newLifetimeWorkerCommand("/usr/bin/true", canonicalRoot, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if worker.Dir != canonicalRoot || !filepath.IsAbs(worker.Dir) {
		t.Fatalf("worker directory = %q, want canonical absolute root %q", worker.Dir, canonicalRoot)
	}
	arguments := strings.Join(worker.Args[1:], " ")
	if !strings.Contains(arguments, "--root "+canonicalRoot+" ") || strings.Contains(arguments, alias) {
		t.Fatalf("worker arguments did not replace symlink root: %q", arguments)
	}
}

func TestLifetimeGuardianRunStatePollsDurableUserStop(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStoreWithSupervisorConfig(t, store, strings.Repeat("c", 32), false, nil, filepath.Join(store.Root, "pueue.yml"))
	defer closeAppTestTask(t, store, td)
	prepareAppBudgetRecords(t, td)

	state, err := loadLifetimeGuardianRunState(store, runnerArguments{TaskID: req.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := state.closeOwner(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if state.userStop == nil {
		t.Fatal("production task guardian has no durable user-stop observer")
	}
	requested, err := state.userStop()
	if err != nil || requested {
		t.Fatalf("initial user stop = %t, error = %v", requested, err)
	}
	permit, err := td.PrepareStop(strings.Repeat("d", 32), "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	requested, err = state.userStop()
	if err != nil || !requested {
		t.Fatalf("durable user stop = %t, error = %v", requested, err)
	}
}

func TestInspectionLifetimeGuardianRunStateHasNoTaskUserStopObserver(t *testing.T) {
	store, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	state, err := loadLifetimeGuardianRunState(store, runnerArguments{
		TaskID:     operation.Request().TaskID,
		Inspection: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := state.closeOwner(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if state.userStop != nil {
		t.Fatal("inspection guardian inherited the task-only user-stop observer")
	}
}

func TestLifetimeControlPairIsDuplex(t *testing.T) {
	parent, childFile, err := newLifetimeControlPair()
	if err != nil {
		t.Fatal(err)
	}
	cleanupLifetimeTestResource(t, parent)
	cleanupLifetimeTestResource(t, childFile)
	childFD, err := unix.Dup(int(childFile.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	child, err := lifetimeConnectionFromFD(childFD)
	if err != nil {
		t.Fatal(err)
	}
	cleanupLifetimeTestResource(t, child)
	if _, err = childFile.Stat(); err != nil {
		t.Fatalf("retained child descriptor is invalid: %v", err)
	}

	parentWrite := []byte("parent")
	childWrite := []byte("child")
	writeErr := make(chan error, 2)
	go func() {
		_, parentWriteErr := parent.Write(parentWrite)
		writeErr <- parentWriteErr
	}()
	gotParent := make([]byte, len(parentWrite))
	if _, err = io.ReadFull(child, gotParent); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, childWriteErr := child.Write(childWrite)
		writeErr <- childWriteErr
	}()
	gotChild := make([]byte, len(childWrite))
	if _, err = io.ReadFull(parent, gotChild); err != nil {
		t.Fatal(err)
	}
	if err = <-writeErr; err != nil {
		t.Fatal(err)
	}
	if err = <-writeErr; err != nil {
		t.Fatal(err)
	}
	if string(gotParent) != string(parentWrite) || string(gotChild) != string(childWrite) {
		t.Fatalf("duplex payloads = %q / %q", gotParent, gotChild)
	}
}

func recordManagedInspectionLifetimeExit(t *testing.T, operation *inspection.Operation, result task.LifetimeWorkerWaitResult, observedAt time.Time) {
	t.Helper()
	admissionfixture.RecordManagedInspectionLifetimeExit(t, operation, result, observedAt)
}

func recordManagedInspectionNotStarted(t *testing.T, operation *inspection.Operation, cause string, observedAt time.Time) {
	t.Helper()
	admissionfixture.RecordManagedInspectionNotStarted(t, operation, cause, observedAt)
}

func recordManagedExactInspectionUnqualifiedExit(t *testing.T, operation *inspection.Operation, result task.LifetimeWorkerWaitResult, cause string, observedAt time.Time) {
	t.Helper()
	admissionfixture.RecordManagedExactInspectionUnqualifiedExit(t, operation, result, cause, observedAt)
}

func TestManagedInspectionDerivesSuccessFromWaitAndTermination(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), created.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	recordManagedInspectionLifetimeExit(t, operation, task.LifetimeWorkerWaitSuccess, created.Add(3*time.Second))
	_, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || !succeeded {
		t.Fatalf("worker proof = succeeded %t, err %v", succeeded, err)
	}
}

func TestManagedInspectionRejectsExactUnqualifiedWorkerSuccess(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	recordManagedExactInspectionUnqualifiedExit(
		t, operation, task.LifetimeWorkerWaitSuccess, task.LifetimeTerminationCompleted, created.Add(time.Second),
	)
	if _, _, err := operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unqualified successful worker error = %v, want evidence fault", err)
	}
}

func TestManagedInspectionNotStartedBudgetIsAdmissionExpiry(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
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
	recordManagedInspectionNotStarted(t, operation, task.LifetimeTerminationBudget, created.Add(2*time.Second))
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if result != nil || succeeded || !errors.Is(err, inspection.ErrAdmissionExpired) {
		t.Fatalf("not-started budget proof = result %#v, succeeded %t, err %v; want expiry", result, succeeded, err)
	}
}

func TestManagedInspectionProofRequiresReceipt(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	recordManagedInspectionLifetimeExit(t, operation, task.LifetimeWorkerWaitExitError, created.Add(time.Second))
	if _, _, err := operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed exit without receipt error = %v, want missing receipt", err)
	}
}

func TestManagedInspectionWaitCanBecomeDurableAfterAdmissionDeadline(t *testing.T) {
	created := time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	start, err := operation.ClaimStart(created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), created.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	recordManagedInspectionLifetimeExit(t, operation, task.LifetimeWorkerWaitSuccess, time.Now().UTC())
	_, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || !succeeded {
		t.Fatalf("late exact Wait = succeeded %t, err %v", succeeded, err)
	}
}

func TestManagedInspectionProofReadHonorsCancellation(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := operation.ReadWorkerResultContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled proof read error = %v, want context canceled", err)
	}
}

func TestManagedInspectionRejectsQueueObservationFallback(t *testing.T) {
	created := time.Now().UTC()
	_, operation := newInspectionWorkerOperationWithExitProof(t, created, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordWorkerExitContext(t.Context(), 42, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed queue fallback was accepted: %v", err)
	}
}
