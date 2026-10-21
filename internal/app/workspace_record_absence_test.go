package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestWorkspaceReconcileRemovalAllowsCleanMissingRelease(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	if err := workspaces.ReconcileRemoval(t.Context(), store, resource, workspaces.OperationOptions{}, false); err != nil {
		t.Fatalf("missing release record was not treated as absent: %v", err)
	}
}

func TestWorkspaceReconcileRemovalRetainsUnlockFaultWithMissingRelease(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	injected := fmt.Errorf("resource release unlock: %w", unix.EIO)
	fault := &workspaceResultUnlockFault{path: filepath.Join(store.Root, "workspaces", resource.Request().WorkspaceID, ".operation.lock"), failure: injected}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	err := workspaces.ReconcileRemoval(t.Context(), store, resource, workspaces.OperationOptions{}, false)
	if !fault.fired {
		t.Fatal("resource operation unlock fault did not fire")
	}
	if !errors.Is(err, injected) || !errors.Is(err, unix.EIO) || workspace.OperationAbsent(err) {
		t.Fatalf("resource unlock failure was classified as release absence: %v", err)
	}
}
