package workspaces

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

func TestAdmissionCleanupRetainsInnerAndBorrowedOuterOwners(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	lease, err := fixture.Store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	})
	request := rootAdmissionDispatchRequest()
	request.WorkspaceID = fixture.Resource.Request().WorkspaceID
	managed, _, prepareErr := PrepareDispatchContext(t.Context(), request, OperationOptions{
		Environment: []string{}, RootAdmission: lease, Runtime: unavailableOperationRuntime{},
	}, fixture.Store, teamDigestTaskID, fixture.Resource.Request().SourceCwd, []byte("work"))
	if managed == nil {
		t.Fatalf("missing admission owner: %v", prepareErr)
	}
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		if err := managed.Close(); err != nil {
			t.Error(err)
		}
	})
	if !errors.Is(prepareErr, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("unexpected preparation: %v", prepareErr)
	}
	releaseFault := errors.New("injected admission use-owner unlock failure")
	fixture.Store.SetFaultInjector(&admissionOwnerUnlockFault{cause: releaseFault})
	finished := managed.FinishContext(t.Context(), fixture.Store, prepareErr, false, nil)
	var cleanup *AdmissionCleanupError
	if !errors.As(finished.CloseError, &cleanup) || cleanup.Admission() != managed || !errors.Is(finished.CloseError, releaseFault) {
		t.Fatalf("failed admission close lost its exact owner/fault: %v", finished.CloseError)
	}
	fixture.Store.SetFaultInjector(nil)
	if owner, err := fixture.Resource.TryAcquireUseOwner(t.Context()); !errors.Is(err, task.ErrLockBusy) {
		if owner != nil {
			if closeErr := owner.Release(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("unresolved admission lost inner owner: %v", err)
	}
	if err := cleanup.Admission().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.PinRootContext(t.Context()); err != nil {
		t.Fatalf("inner cleanup released borrowed outer admission: %v", err)
	}
}

type admissionOwnerUnlockFault struct {
	taskdir.BaseFaultInjector
	cause error
}

func (*admissionOwnerUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*admissionOwnerUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *admissionOwnerUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && operation&unix.LOCK_UN != 0 {
		return f.cause
	}
	return nil
}
