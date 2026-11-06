package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"golang.org/x/sys/unix"
)

var errCheckpointAdmissionProbe = errors.New("checkpoint admission probe completed")

type checkpointAdmissionProbe struct {
	store *taskdir.Store
}

func (*checkpointAdmissionProbe) ManagedRunner() (string, error) {
	return "", errCheckpointAdmissionProbe
}

func (*checkpointAdmissionProbe) PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error) {
	return "", nil, errCheckpointAdmissionProbe
}

func (*checkpointAdmissionProbe) OpenSaved(context.Context, *taskdir.Store, task.SupervisorRef, infrastructure.SavedBindingSource) (*pueue.Client, error) {
	return nil, errCheckpointAdmissionProbe
}

type workspaceAdmissionUnlockFault struct {
	taskdir.BaseFaultInjector
	cause error
}

func (*workspaceAdmissionUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*workspaceAdmissionUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *workspaceAdmissionUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && operation&unix.LOCK_UN != 0 {
		return f.cause
	}
	return nil
}

var _ workspaces.OperationRuntime = (*checkpointAdmissionProbe)(nil)

func TestWorkspaceRecoveryDoesNotHideFaultJoinedWithBusy(t *testing.T) {
	fault := errors.New("injected failed use-owner maintenance release")
	for _, test := range []struct {
		name, kind string
		fault      error
	}{{"wake", "wake", fault}, {"guardian", "guardian", fault}, {"wake-deadline", "wake", context.DeadlineExceeded}, {"guardian-deadline", "guardian", context.DeadlineExceeded}} {
		t.Run(test.name, func(t *testing.T) { testWorkspaceBusyCleanup(t, test.kind, test.fault) })
	}
}

func testWorkspaceBusyCleanup(t *testing.T, kind string, fault error) {
	t.Helper()
	store, resource, use := newManagedTaskUse(t)
	holdWorkspaceOtherUseOwner(t, store, resource.Request().WorkspaceID)
	injector := &workspaceBusyReleaseFault{cause: fault}
	store.SetFaultInjector(injector)
	var retained *workspaceCommandCleanupError
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if retained != nil {
			if cleanupErr := retained.Cleanup(); cleanupErr != nil {
				t.Error(cleanupErr)
			}
		}
	})
	var resultErr error
	if kind == "wake" {
		resultErr = wakeManagedTaskWorkspace(t.Context(), store, use.td, []string{})
	} else {
		resultErr = reconcileWorkspaceAfterGuardian(store, runnerArguments{TaskID: use.td.TaskID}, Dependencies{Environment: []string{}})
	}
	if !errors.As(resultErr, &retained) || retained.RetainedStore() != store {
		t.Fatalf("failed unwind lost its exact resource/Store cleanup owner: %v", resultErr)
	}
	partial := requireWorkspaceLifetimeFault(t, store, resultErr, fault, injector.unlocks)
	store.SetFaultInjector(nil)
	if cleanupErr := retained.Cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if partial.Lease().CleanupPending() || retained.RetainedStore() != nil {
		t.Fatal("explicit child-first cleanup left owned native handles")
	}
	if workspaces.PendingError(errors.Join(workspace.ErrPending, resultErr)) || workspaceViewRetryable(resultErr) {
		t.Fatal("resolved historical cleanup fault became pending")
	}
}

func requireWorkspaceLifetimeFault(t *testing.T, store *taskdir.Store, resultErr, fault error, unlocks int) *taskdir.LifetimeOwnerCleanupError {
	t.Helper()
	if !errors.Is(resultErr, task.ErrLockBusy) || !errors.Is(resultErr, fault) || workspaces.PendingError(resultErr) || workspaceViewRetryable(resultErr) {
		t.Fatalf("fault joined with native owner contention was hidden: %v", resultErr)
	}
	if workspaces.PendingError(errors.Join(workspace.ErrPending, resultErr)) {
		t.Fatalf("pending marker hid a typed cleanup fault: %v", resultErr)
	}
	if unlocks != 1 {
		t.Fatalf("failed unwind retried implicitly: %d unlocks", unlocks)
	}
	var partial *taskdir.LifetimeOwnerCleanupError
	if !errors.As(resultErr, &partial) || !partial.Lease().RetainsStore(store) {
		t.Fatalf("lost the exact partial lifetime owner: %v", resultErr)
	}
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("borrowed Store closed behind partial owner: %v", pinErr)
	}
	return partial
}

func holdWorkspaceOtherUseOwner(t *testing.T, store *taskdir.Store, workspaceID string) {
	t.Helper()
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	resources := &workspaceCommandResources{store: other, ownsStore: true}
	t.Cleanup(func() {
		if closeErr := resources.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	resource, err := workspace.LoadResource(t.Context(), other, workspaceID)
	resources.resource = resource
	if err != nil {
		t.Fatal(err)
	}
	owner, err := resource.AcquireUseOwner(t.Context())
	resources.useOwner = owner
	if err != nil {
		t.Fatal(err)
	}
}

type workspaceBusyReleaseFault struct {
	taskdir.BaseFaultInjector
	cause   error
	armed   bool
	unlocks int
}

func (*workspaceBusyReleaseFault) OnLockFileBarrier(string) error      { return nil }
func (*workspaceBusyReleaseFault) OnLockParentDirBarrier(string) error { return nil }
func (f *workspaceBusyReleaseFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && operation&unix.LOCK_EX != 0 {
		f.armed = true
	}
	if f.armed && filepath.Base(path) == ".maintenance.lock" && operation&unix.LOCK_UN != 0 {
		f.unlocks++
		return f.cause
	}
	return nil
}

func appCleanupLifetimeLease(t *testing.T, store *taskdir.Store) *taskdir.LifetimeOwnerLease {
	t.Helper()
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := errors.Join(lease.Release(), control.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
