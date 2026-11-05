package workspaces

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func rootAdmissionDispatchRequest() DispatchRequest {
	return DispatchRequest{
		Isolation: "git-worktree", Provider: "fixture:test", RunnerOwnership: task.RunnerOwnershipManaged,
		Config: task.TaskConfig{Permission: "read-only", Budget: "1m0s"},
	}
}

func TestPrepareDispatchRootAdmissionPrecedesResourceCreation(t *testing.T) {
	store := newRootQuiescenceStore(t)
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	counter := &rootAdmissionAcquisitionCounter{onShared: cancel}
	store.SetFaultInjector(counter)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	managed, _, err := PrepareDispatchContext(ctx, rootAdmissionDispatchRequest(), OperationOptions{Environment: []string{}, Runtime: unavailableOperationRuntime{}}, store, teamDigestTaskID, filepath.Join(t.TempDir(), "source"), []byte("work"))
	if managed != nil {
		t.Cleanup(func() {
			if closeErr := managed.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	prepareErr := err
	if counter.shared == 0 {
		t.Fatal("resource preparation did not attempt native root SH admission")
	}
	ids, err := store.WorkspaceResourceIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("resource preparation escaped admission: %v, %v", ids, err)
	}
	if !errors.Is(prepareErr, context.Canceled) {
		t.Fatalf("prepare under root EX = %v, want cancellation after native admission contention", prepareErr)
	}
}

type rootAdmissionAcquisitionCounter struct {
	taskdir.BaseFaultInjector
	shared     int
	onShared   func()
	releaseErr error
}

func (*rootAdmissionAcquisitionCounter) OnLockFileBarrier(string) error      { return nil }
func (*rootAdmissionAcquisitionCounter) OnLockParentDirBarrier(string) error { return nil }
func (f *rootAdmissionAcquisitionCounter) OnFlock(path string, operation int) error {
	if filepath.Base(path) != taskdir.RootAdmissionLockName {
		return nil
	}
	if operation&unix.LOCK_UN != 0 {
		return f.releaseErr
	}
	if operation&unix.LOCK_SH != 0 {
		f.shared++
		if f.onShared != nil {
			f.onShared()
		}
	}
	return nil
}

func TestPrepareDispatchBorrowsRootAdmissionThroughFinish(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	lease, err := fixture.Store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	counter := &rootAdmissionAcquisitionCounter{}
	fixture.Store.SetFaultInjector(counter)
	t.Cleanup(func() { fixture.Store.SetFaultInjector(nil) })
	request := rootAdmissionDispatchRequest()
	request.WorkspaceID = fixture.Resource.Request().WorkspaceID
	managed, _, err := PrepareDispatchContext(t.Context(), request, OperationOptions{
		Environment: []string{}, RootAdmission: lease, Runtime: unavailableOperationRuntime{},
	}, fixture.Store, teamDigestTaskID, fixture.Resource.Request().SourceCwd, []byte("work"))
	if managed == nil {
		t.Fatalf("missing owned cleanup handle: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := managed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if !errors.Is(err, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("prepare = %v, want runtime boundary fault", err)
	}
	finished := managed.FinishContext(t.Context(), fixture.Store, err, false, nil)
	if finished.AbortError != nil || finished.CloseError != nil {
		t.Fatalf("finish abort=%v close=%v", finished.AbortError, finished.CloseError)
	}
	if counter.shared != 0 {
		t.Fatalf("borrowed lease reacquired root SH %d times", counter.shared)
	}
	if _, err := lease.PinRootContext(t.Context()); err != nil {
		t.Fatalf("child released borrowed root admission: %v", err)
	}
	if acquired, err := fixture.Store.TryAcquireRootAdmissionExclusiveContext(t.Context()); !errors.Is(err, task.ErrLockBusy) {
		if acquired != nil {
			if closeErr := acquired.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("parent no longer excludes EX: %v", err)
	}
}

func TestPrepareDispatchFailedResourceRetainsRootCleanupOwner(t *testing.T) {
	store := newRootQuiescenceStore(t)
	request := rootAdmissionDispatchRequest()
	request.WorkspaceID = teamDigestTaskID
	managed, _, err := PrepareDispatchContext(t.Context(), request, OperationOptions{Environment: []string{}}, store, teamDigestTaskID, t.TempDir(), []byte("work"))
	if err == nil || managed == nil {
		t.Fatalf("failed resource did not return admission cleanup: %v, %v", managed, err)
	}
	t.Cleanup(func() {
		if closeErr := managed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	finished := managed.FinishContext(t.Context(), store, err, false, nil)
	if finished.CloseError != nil {
		t.Fatal(finished.CloseError)
	}
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatalf("finish retained owned root admission: %v", err)
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRecoveryRootAdmissionPrecedesJournalOpen(t *testing.T) {
	store := newRootQuiescenceStore(t)
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	cases := []struct {
		name string
		want error
		run  func(context.Context) error
	}{
		{"observe", context.Canceled, func(ctx context.Context) error {
			_, err := ObserveCheckpoint(ctx, store, OperationOptions{}, teamDigestTaskID)
			return err
		}},
		{"observe removal", context.Canceled, func(ctx context.Context) error {
			_, _, err := ObserveCheckpointRemoval(ctx, store, OperationOptions{}, teamDigestTaskID, teamDigestTaskID)
			return err
		}},
		{"remove", context.Canceled, func(ctx context.Context) error {
			_, err := BeginCheckpointRemoval(ctx, store, OperationOptions{}, teamDigestTaskID, time.Minute)
			return err
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			observer, cancelObserver := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelObserver()
			// Cancel the canonical control owner at the native gate. A short
			// observer cannot cancel an already owned recovery transaction.
			control, cancelControl := workspaceSchedulingControl(observer, OperationOptions{})
			defer cancelControl()
			counter := &rootAdmissionAcquisitionCounter{onShared: cancelControl}
			store.SetFaultInjector(counter)
			t.Cleanup(func() { store.SetFaultInjector(nil) })
			if err := test.run(control); !errors.Is(err, test.want) || counter.shared == 0 {
				t.Fatalf("recovery under root EX = %v after %d native SH attempts", err, counter.shared)
			}
		})
	}
}

func TestCheckpointRecoveryRetainsFailedAdmissionCleanup(t *testing.T) {
	store := newRootQuiescenceStore(t)
	releaseFault := errors.New("injected root admission unlock failure")
	store.SetFaultInjector(&rootAdmissionAcquisitionCounter{releaseErr: releaseFault})
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	_, err := BeginCheckpointRemoval(t.Context(), store, OperationOptions{}, teamDigestTaskID, time.Minute)
	var cleanup *supervisorcore.RootAdmissionCleanupError
	if !errors.As(err, &cleanup) || cleanup.Lease() == nil {
		t.Fatalf("failed release lost its cleanup handle: %v", err)
	}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if releaseErr := cleanup.Lease().Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, releaseFault) {
		t.Fatalf("recovery lost work/release errors: %v", err)
	}
	if exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context()); !errors.Is(err, task.ErrLockBusy) {
		if exclusive != nil {
			if closeErr := exclusive.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("unresolved release lost SH custody: %v", err)
	}
	store.SetFaultInjector(nil)
	if err := cleanup.Lease().Release(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointCanceledObserverKeepsOwnedRecoveryContext(t *testing.T) {
	store := newRootQuiescenceStore(t)
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	operation := newRootQuiescenceCheckpoint(t, store)
	deadline := operation.Deadline()
	observer, cancel := context.WithCancel(t.Context())
	cancel()
	description, err := ObserveCheckpoint(observer, store, OperationOptions{RootAdmission: lease}, operation.Request().OperationID)
	if err != nil || description.ID != operation.Request().OperationID || description.State != workspace.CheckpointUnavailable {
		t.Fatalf("canceled observer canceled owned checkpoint recovery: %+v, %v", description, err)
	}
	if operation.Deadline() != deadline {
		t.Fatal("observation renewed the stored helper deadline")
	}
	if _, pinErr := lease.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("observation released borrowed root admission: %v", pinErr)
	}
}
