package workspaces

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestPrepareDispatchRetainsPartialUseOwnerThroughFinish(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	holdOtherWorkspaceUse(t, fixture.Store, fixture.Resource.Request().WorkspaceID)
	observer, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &partialUseOwnerFault{cause: context.DeadlineExceeded, cancel: cancel, ownerID: fixture.Resource.Request().WorkspaceID}
	fixture.Store.SetFaultInjector(fault)
	var managed *Admission
	var partial *taskdir.LifetimeOwnerCleanupError
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		if partial != nil {
			if releaseErr := partial.Lease().Release(); releaseErr != nil {
				t.Error(releaseErr)
				return
			}
		}
		if closeErr := managed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	request := rootAdmissionDispatchRequest()
	request.WorkspaceID = fixture.Resource.Request().WorkspaceID
	var err error
	managed, _, err = PrepareDispatchContext(observer, request, OperationOptions{Environment: []string{}}, fixture.Store, teamDigestTaskID, fixture.Resource.Request().SourceCwd, []byte("work"))
	if managed == nil || !errors.As(err, &partial) || managed.owner != partial.Lease() {
		t.Fatalf("dispatch lost its partial native owner: %v", err)
	}
	finished := managed.FinishContext(observer, fixture.Store, err, false, nil)
	var retained *AdmissionCleanupError
	if !errors.As(finished.CloseError, &retained) || retained.Admission() != managed || !managed.CleanupPending() {
		t.Fatalf("Finish lost its cleanup-only admission: %+v", finished)
	}
	if fault.unlocks != 1 || PendingError(finished.CloseError) {
		t.Fatalf("Finish retried or classified failed unwind pending: unlocks=%d error=%v", fault.unlocks, finished.CloseError)
	}
	fixture.Store.SetFaultInjector(nil)
	if closeErr := retained.Admission().Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if managed.CleanupPending() || partial.Lease().CleanupPending() {
		t.Fatal("explicit admission cleanup left native ownership")
	}
}

func TestContinuationRetainsPartialResourceOwner(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	holdOtherWorkspaceUse(t, fixture.Store, fixture.Resource.Request().WorkspaceID)
	root, err := fixture.Store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := root.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	fault := &partialUseOwnerFault{cause: context.DeadlineExceeded, ownerID: fixture.Resource.Request().WorkspaceID}
	fixture.Store.SetFaultInjector(fault)
	var retained *AdmissionCleanupError
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		if retained != nil {
			if closeErr := retained.Admission().Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	predecessor := task.TaskRecord{Workspace: &task.WorkspaceBinding{ID: fixture.Resource.Request().WorkspaceID}}
	view, continuationErr := PrepareContinuation(t.Context(), fixture.Store, predecessor, OperationOptions{RootAdmission: root})
	if view != nil || !errors.As(continuationErr, &retained) || PendingError(continuationErr) || fault.unlocks != 1 {
		t.Fatalf("continuation lost failed unwind: view=%+v error=%v unlocks=%d", view, continuationErr, fault.unlocks)
	}
	if _, pinErr := root.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("borrowed root was released: %v", pinErr)
	}
}

func TestCheckpointRecoveryRetainsPartialResourceAndOwnedRoot(t *testing.T) {
	fixture := newReservedManagedCheckpoint(t)
	store := fixture.recovery.Store
	holdOtherWorkspaceUse(t, store, fixture.recovery.Resource.Request().WorkspaceID)
	fault := &partialUseOwnerFault{cause: context.DeadlineExceeded, ownerID: fixture.recovery.Resource.Request().WorkspaceID}
	store.SetFaultInjector(fault)
	var retained *AdmissionCleanupError
	var parent *taskdir.RootAdmissionLease
	var partial *taskdir.LifetimeOwnerCleanupError
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if partial != nil {
			if releaseErr := partial.Lease().Release(); releaseErr != nil {
				t.Error(releaseErr)
				return
			}
		}
		if retained != nil {
			if closeErr := retained.Admission().Close(); closeErr != nil {
				t.Error(closeErr)
				return
			}
		}
		if releaseErr := parent.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	_, err := ObserveCheckpoint(t.Context(), store, OperationOptions{}, fixture.operation.Request().OperationID)
	parent = retainedRootAdmission(err)
	hasPartial := errors.As(err, &partial)
	if !hasPartial || !errors.As(err, &retained) || parent == nil || fault.unlocks != 1 || PendingError(err) {
		t.Fatalf("checkpoint lost resource/root ownership: %v (unlocks %d)", err, fault.unlocks)
	}
	if _, pinErr := parent.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("owned root released before child cleanup: %v", pinErr)
	}
}

func holdOtherWorkspaceUse(t *testing.T, store *taskdir.Store, workspaceID string) {
	t.Helper()
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	held := &Admission{}
	t.Cleanup(func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Error(closeErr)
			return
		}
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	held.rootAdmission, err = other.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	held.resource, err = workspace.LoadResource(t.Context(), other, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	held.owner, err = held.resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

type partialUseOwnerFault struct {
	taskdir.BaseFaultInjector
	cause   error
	ownerID string
	cancel  context.CancelFunc
	armed   bool
	unlocks int
}

func (*partialUseOwnerFault) OnLockFileBarrier(string) error      { return nil }
func (*partialUseOwnerFault) OnLockParentDirBarrier(string) error { return nil }
func (f *partialUseOwnerFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && filepath.Base(filepath.Dir(path)) == f.ownerID && operation&unix.LOCK_EX != 0 {
		f.armed = true
		if f.cancel != nil {
			f.cancel()
		}
	}
	if f.armed && filepath.Base(path) == ".maintenance.lock" && operation&unix.LOCK_UN != 0 {
		f.unlocks++
		return f.cause
	}
	return nil
}

func retainedRootAdmission(cause error) *taskdir.RootAdmissionLease {
	var retained *supervisorcore.RootAdmissionCleanupError
	if errors.As(cause, &retained) {
		return retained.Lease()
	}
	return nil
}

// This isolated guard test checks native resource custody only. It grants no
// closing-commit authority; the application separately supplies root fences.
func TestRootQuiescenceRetainsPartialOwnerWithoutRetry(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	holdOtherWorkspaceUse(t, fixture.Store, fixture.Resource.Request().WorkspaceID)
	fault := &partialUseOwnerFault{cause: context.DeadlineExceeded, ownerID: fixture.Resource.Request().WorkspaceID}
	fixture.Store.SetFaultInjector(fault)
	var guard *RootQuiescence
	var partial *taskdir.LifetimeOwnerCleanupError
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		if partial != nil {
			if releaseErr := partial.Lease().Release(); releaseErr != nil {
				t.Error(releaseErr)
				return
			}
		}
		if closeErr := guard.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var err error
	guard, err = AcquireRootQuiescence(t.Context(), fixture.Store)
	if guard == nil || !errors.As(err, &partial) || len(guard.resources) != 1 || guard.resources[0].owner != partial.Lease() {
		t.Fatalf("quiescence lost partial owner/resource: %v", err)
	}
	if fault.unlocks != 1 || PendingError(err) || !partial.Lease().RetainsStore(fixture.Store) {
		t.Fatalf("quiescence retried or hid failed unwind: %v, unlocks=%d", err, fault.unlocks)
	}
	fixture.Store.SetFaultInjector(nil)
	if closeErr := guard.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if partial.Lease().CleanupPending() || len(guard.resources) != 0 {
		t.Fatal("explicit guard cleanup left native custody")
	}
}
