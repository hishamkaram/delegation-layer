package workspaces

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestWorkspaceEnqueuePermissionRejectsClosedReceiptReplay(t *testing.T) {
	store, operation := workspacefixture.NewOperation(t)
	if receiptErr := operation.RecordReceipt(t.Context(), 9); receiptErr != nil {
		t.Fatal(receiptErr)
	}
	workspacefixture.SeedRealOperationClosure(t, store, operation, ReadOperationProof)
	if receiptErr := operation.RecordReceipt(t.Context(), 9); receiptErr != nil {
		t.Fatalf("closed receipt replay = %v", receiptErr)
	}
	if permissionErr := operation.CheckLaunchPermission(t.Context()); !errors.Is(permissionErr, task.ErrTerminalTask) {
		t.Fatalf("closed canonical launch permission = %v", permissionErr)
	}
	if permissionErr := workspaceOperationLaunchPermission(t.Context(), operation); !errors.Is(permissionErr, workspace.ErrPending) || !PendingError(permissionErr) {
		t.Fatalf("closed receipt granted enqueue permission: %v", permissionErr)
	}
	unlockFault := errors.New("injected operation journal unlock failure")
	store.SetFaultInjector(&permissionUnlockFault{cause: unlockFault})
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	err := workspaceOperationLaunchPermission(t.Context(), operation)
	if !errors.Is(err, unlockFault) || !errors.Is(err, task.ErrTerminalTask) || PendingError(err) {
		t.Fatalf("current permission hid its storage fault: %v", err)
	}
	store.SetFaultInjector(nil)
	if closeErr := operation.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

type permissionUnlockFault struct {
	taskdir.BaseFaultInjector
	cause error
}

func (permissionUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (permissionUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f permissionUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".operation.lock" && operation&unix.LOCK_UN != 0 {
		return f.cause
	}
	return nil
}
