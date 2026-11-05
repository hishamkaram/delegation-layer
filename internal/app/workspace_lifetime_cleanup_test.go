package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestWorkspaceNestedLifetimeCleanupPreservesResourceOwner(t *testing.T) {
	store, resource := workspacefixture.NewResource(t)
	resources := &workspaceCommandResources{store: store, resource: resource}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := resources.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, _, acquireErr := resources.acquire(t.Context(), nil, pueue.Options{}); acquireErr != nil {
		t.Fatal(acquireErr)
	}
	parent, err := resource.AcquireUseOwner(t.Context())
	resources.useOwner = parent
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	control, err := store.OpenWorkspaceOperation(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	var child *taskdir.LifetimeOwnerLease
	t.Cleanup(func() { cleanupNestedWorkspaceTest(t, store, resources, control, parent, child) })
	observer, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &nestedWorkspaceOwnerFault{path: filepath.Join(workspacefixture.OperationDirectory(store, id), ".lifetime-owner.lock"), cancel: cancel}
	store.SetFaultInjector(fault)
	var childErr error
	child, childErr = control.TryAcquireLifetimeOwnerContext(observer)
	var partial *taskdir.LifetimeOwnerCleanupError
	if !errors.As(childErr, &partial) || child != partial.Lease() {
		t.Fatalf("nested acquisition did not retain its exact cleanup lease: %v", childErr)
	}
	retainedErr := resources.closeAfter(childErr)
	retained := requireNestedWorkspaceOwnership(t, resources, parent, child, retainedErr, fault.unlocks)
	store.SetFaultInjector(nil)
	if cleanupErr := retained.Cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if parent.CleanupPending() || child.CleanupPending() || retained.RetainedStore() != nil {
		t.Fatal("explicit cleanup left nested native ownership")
	}
}

type nestedWorkspaceOwnerFault struct {
	taskdir.BaseFaultInjector
	path    string
	cancel  context.CancelFunc
	unlocks int
}

func (*nestedWorkspaceOwnerFault) OnLockFileBarrier(string) error      { return nil }
func (*nestedWorkspaceOwnerFault) OnLockParentDirBarrier(string) error { return nil }
func (f *nestedWorkspaceOwnerFault) OnFlock(path string, operation int) error {
	if path != f.path {
		return nil
	}
	if operation&unix.LOCK_EX != 0 {
		f.cancel()
	}
	if operation&unix.LOCK_UN != 0 {
		f.unlocks++
		return context.DeadlineExceeded
	}
	return nil
}

func cleanupNestedWorkspaceTest(t *testing.T, store *taskdir.Store, resources *workspaceCommandResources, control *taskdir.ControlDir, parent, child *taskdir.LifetimeOwnerLease) {
	t.Helper()
	store.SetFaultInjector(nil)
	if releaseErr := child.Release(); releaseErr != nil {
		t.Error(releaseErr)
		return
	}
	if releaseErr := parent.Release(); releaseErr != nil {
		t.Error(releaseErr)
		return
	}
	if closeErr := resources.close(); closeErr != nil {
		t.Error(closeErr)
		return
	}
	if closeErr := control.Close(); closeErr != nil {
		t.Error(closeErr)
	}
}

func requireNestedWorkspaceOwnership(t *testing.T, resources *workspaceCommandResources, parent, child *taskdir.LifetimeOwnerLease, cause error, unlocks int) *workspaceCommandCleanupError {
	t.Helper()
	var retained *workspaceCommandCleanupError
	if !errors.As(cause, &retained) || retained.RetainedStore() != resources.store || resources.childOwner != child || resources.useOwner != parent {
		t.Fatalf("nested child overwrote resource owner or lost Store: %v", cause)
	}
	if unlocks != 1 || !parent.CleanupPending() || !child.CleanupPending() {
		t.Fatalf("cleanup retried child or released parent: unlocks=%d parent=%t child=%t", unlocks, parent.CleanupPending(), child.CleanupPending())
	}
	return retained
}
