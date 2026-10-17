package app

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

type workspaceResultUnlockFault struct {
	taskdir.BaseFaultInjector
	path    string
	failure error
	fired   bool
}

func (f *workspaceResultUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (f *workspaceResultUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *workspaceResultUnlockFault) OnFlock(path string, operation int) error {
	if path == f.path && operation == unix.LOCK_UN && !f.fired {
		f.fired = true
		return f.failure
	}
	return nil
}

func TestWorkspaceMissingResultDoesNotHideTransactionFailure(t *testing.T) {
	store, operation := workspacefixture.NewOperation(t)
	injected := errors.New("result transaction unlock failed")
	fault := &workspaceResultUnlockFault{path: filepath.Join(store.Root, "workspace-operations", operation.Request().OperationID, ".operation.lock"), failure: injected}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	_, err := operation.Result(t.Context())
	if !fault.fired || !errors.Is(err, injected) || workspace.OperationAbsent(err) {
		t.Fatalf("transaction failure was classified as result absence: %v", err)
	}
}
