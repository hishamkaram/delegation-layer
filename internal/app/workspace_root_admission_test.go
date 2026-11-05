package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestWorkspaceStatusRootAdmissionPrecedesRecovery(t *testing.T) {
	store, resource := workspacefixture.NewResource(t)
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	result := workspaceCommand(Arguments{Root: store.Root, WorkspaceAction: "status", WorkspaceID: resource.Request().WorkspaceID}, Dependencies{
		Environment: []string{}, SupervisorOptions: pueue.Options{ObservationTimeout: time.Millisecond},
	})
	if result.code != 1 || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("workspace recovery bypassed root EX: exit=%d error=%v", result.code, result.err)
	}
}

func TestManagedCheckpointCommandHoldsOuterRootAdmission(t *testing.T) {
	store, resource := workspacefixture.NewResource(t)
	runtime := &checkpointAdmissionProbe{store: store}
	options := workspaces.OperationOptions{Runtime: runtime, Environment: []string{}}
	result := runCheckpointCommand(Arguments{
		Root: store.Root, CheckpointAction: checkpointCreateAction, WorkspaceID: resource.Request().WorkspaceID,
	}, storeDependencies{}, options)
	if !runtime.rootHeld || !errors.Is(result.err, errCheckpointAdmissionProbe) {
		t.Fatalf("checkpoint preparation lost outer root admission: held=%t error=%v", runtime.rootHeld, result.err)
	}
	requireRootAdmissionExclusiveAvailable(t, store)
}

func TestWorkspaceAndCheckpointListsDoNotAdmitRootWork(t *testing.T) {
	store, _ := workspacefixture.NewResource(t)
	lockPath := filepath.Join(store.Root, taskdir.RootAdmissionLockName)
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture already has root admission inode: %v", err)
	}
	listed := workspaceCommand(Arguments{Root: store.Root, WorkspaceAction: "list"}, Dependencies{Environment: []string{}})
	if listed.code != 0 {
		t.Fatalf("workspace metadata list = %+v", listed)
	}
	checkpoints := runCheckpointCommand(Arguments{Root: store.Root, CheckpointAction: "list", CheckpointLimit: 10}, storeDependencies{}, workspaces.OperationOptions{})
	if checkpoints.code != 0 {
		t.Fatalf("checkpoint metadata list = %+v", checkpoints)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive lists created admission state: %v", err)
	}
}

func TestWorkspaceRootReleaseFailureRetainsExactStore(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	resources := &workspaceCommandResources{store: store, ownsStore: true}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := resources.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, _, acquireErr := resources.acquire(t.Context(), nil, pueue.Options{}); acquireErr != nil {
		t.Fatal(acquireErr)
	}
	fault := errors.New("injected workspace root admission unlock failure")
	store.SetFaultInjector(&workspaceAdmissionUnlockFault{cause: fault})
	cleanupErr := resources.closeAfter(nil)
	var retained *workspaceCommandCleanupError
	if !errors.As(cleanupErr, &retained) || retained.RetainedStore() != store || !errors.Is(cleanupErr, fault) {
		t.Fatalf("root release lost its exact owner/store: %v", cleanupErr)
	}
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("store closed behind failed root release: %v", pinErr)
	}
	if exclusive, exclusiveErr := store.TryAcquireRootAdmissionExclusiveContext(t.Context()); !errors.Is(exclusiveErr, task.ErrLockBusy) {
		if exclusive != nil {
			if releaseErr := exclusive.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		}
		t.Fatalf("failed root release lost native custody: %v", exclusiveErr)
	}
	store.SetFaultInjector(nil)
	if retryErr := retained.Cleanup(); retryErr != nil {
		t.Fatal(retryErr)
	}
	if retained.RetainedStore() != nil {
		t.Fatal("resolved cleanup still claims store custody")
	}
}

var errCheckpointAdmissionProbe = errors.New("checkpoint admission probe completed")

type checkpointAdmissionProbe struct {
	store    *taskdir.Store
	rootHeld bool
}

func (p *checkpointAdmissionProbe) ManagedRunner() (string, error) {
	exclusive, err := p.store.TryAcquireRootAdmissionExclusiveContext(context.Background())
	p.rootHeld = errors.Is(err, task.ErrLockBusy)
	if exclusive != nil {
		return "", errors.Join(errCheckpointAdmissionProbe, err, exclusive.Release())
	}
	return "", errCheckpointAdmissionProbe
}

func (*checkpointAdmissionProbe) PrepareManaged(context.Context, *taskdir.Store, string) (string, *pueue.Client, error) {
	return "", nil, errCheckpointAdmissionProbe
}

func (*checkpointAdmissionProbe) OpenSaved(context.Context, *taskdir.Store, task.SupervisorRef, infrastructure.SavedBindingSource) (*pueue.Client, error) {
	return nil, errCheckpointAdmissionProbe
}

func requireRootAdmissionExclusiveAvailable(t *testing.T, store *taskdir.Store) {
	t.Helper()
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if err != nil {
		t.Fatalf("command retained root admission: %v", err)
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}
}

type workspaceAdmissionUnlockFault struct {
	taskdir.BaseFaultInjector
	cause error
}

func (*workspaceAdmissionUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*workspaceAdmissionUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *workspaceAdmissionUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == taskdir.RootAdmissionLockName && operation&unix.LOCK_UN != 0 {
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
	if _, _, acquireErr := resources.acquire(t.Context(), nil, pueue.Options{}); acquireErr != nil {
		t.Fatal(acquireErr)
	}
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
