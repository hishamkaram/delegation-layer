package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceCanceledWatchDoesNotBeginCaptureReplay(t *testing.T) {
	for _, test := range []struct {
		name    string
		expired bool
	}{{"canceled", false}, {"deadline", true}} {
		t.Run(test.name, func(t *testing.T) {
			fixture, verification, capture, deps := prepareClosedVerificationCapture(t)
			observer, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if test.expired {
				cancel()
				observer, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			cancel()
			var commands atomic.Int32
			deps.SupervisorOptions.Observer = func(pueue.CommandEvent) { commands.Add(1) }
			err := watchManagedWorkspace(observer, fixture.Store, fixture.Resource, deps, Arguments{Watch: time.Second})
			if !errors.Is(err, want) || (test.expired && !errors.Is(err, workspace.ErrPending)) {
				t.Fatalf("already stopped observer began replay: %v", err)
			}
			if _, err = fixture.Resource.CaptureReceipt(t.Context(), capture.OperationID); !workspace.OperationAbsent(err) {
				t.Fatalf("already stopped observer published a capture receipt: %v", err)
			}
			if _, err = fixture.Resource.Reservation(t.Context()); err != nil {
				t.Fatalf("already stopped observer changed the reservation: %v", err)
			}
			if commands.Load() != 0 {
				t.Fatalf("already stopped observer contacted the supervisor %d times", commands.Load())
			}
			assertWorkspaceReplayRequestUnchanged(t, fixture.Store, verification)
			assertWorkspaceReplayRequestUnchanged(t, fixture.Store, capture)
		})
	}
}

type workspaceReplayObserverCancellation struct {
	taskdir.BaseFaultInjector
	path   string
	cancel context.CancelFunc
	fired  bool
}

func (f *workspaceReplayObserverCancellation) OnLockFileBarrier(path string) error {
	if path == f.path && !f.fired {
		f.fired = true
		f.cancel()
	}
	return nil
}

func (*workspaceReplayObserverCancellation) OnLockParentDirBarrier(string) error { return nil }
func (*workspaceReplayObserverCancellation) OnFlock(string, int) error           { return nil }

func prepareClosedVerificationCapture(t *testing.T) (*testsupport.RecoveryFixture, workspace.OperationRequest, workspace.OperationRequest, Dependencies) {
	t.Helper()
	fixture := testsupport.NewRecoveryFixture(t)
	verification, subject, deps := workspaceVerificationRecoveryAuthorization(t, fixture)
	operation, err := workspace.LoadOperation(t.Context(), fixture.Store, verification.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := fixture.Resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = workspaces.RecordVerificationUse(t.Context(), fixture.Resource, reservation, operation); err != nil {
		t.Fatal(err)
	}
	seedRealWorkspaceOperationClosure(t, fixture.Store, operation)
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	capture := testsupport.OpenRecoveryOperation(t, fixture, strings.Repeat("f", 32), workspace.OperationCapture, subject)
	if _, err = fixture.Resource.AssociateOperation(t.Context(), workspace.CapturePurpose(reservation.Sequence), capture.Request()); err != nil {
		t.Fatal(err)
	}
	seedRealWorkspaceOperationClosure(t, fixture.Store, capture)
	testsupport.RecordRecoveredCaptureCandidate(t, fixture, capture)
	request := capture.Request()
	if err = capture.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.Restart(t)
	testsupport.RecoveryUseOwner(t, fixture)
	return fixture, verification, request, deps
}

func TestWorkspaceClosedVerificationCaptureSurvivesObserverCancellationDuringReplay(t *testing.T) {
	fixture, verification, capture, deps := prepareClosedVerificationCapture(t)
	observer, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &workspaceReplayObserverCancellation{
		path: filepath.Join(fixture.Store.Root, "workspace-operations", capture.OperationID, ".operation.lock"), cancel: cancel,
	}
	fixture.Store.SetFaultInjector(fault)
	t.Cleanup(func() { fixture.Store.SetFaultInjector(nil) })
	var commands atomic.Int32
	deps.SupervisorOptions.Observer = func(pueue.CommandEvent) { commands.Add(1) }
	err := reconcileWorkspaceUse(observer, fixture.Store, fixture.Resource, deps)
	if !fault.fired || observer.Err() != context.Canceled || err != nil {
		t.Fatalf("closed capture replay followed the observer cancellation: fired=%t observer=%v reconcile=%v", fault.fired, observer.Err(), err)
	}
	receipt, err := fixture.Resource.CaptureReceipt(t.Context(), capture.OperationID)
	if err != nil || receipt.Status != "captured" || receipt.Subject != capture.Subject {
		t.Fatalf("closed verification capture was not sealed: %+v, %v", receipt, err)
	}
	if _, err = fixture.Resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("sealed capture retained its usage reservation: %v", err)
	}
	if commands.Load() != 0 {
		t.Fatalf("closed capture replay contacted the supervisor %d times", commands.Load())
	}
	assertWorkspaceReplayRequestUnchanged(t, fixture.Store, verification)
	assertWorkspaceReplayRequestUnchanged(t, fixture.Store, capture)
}

func assertWorkspaceReplayRequestUnchanged(t *testing.T, store *taskdir.Store, request workspace.OperationRequest) {
	t.Helper()
	operation, err := workspace.LoadOperation(t.Context(), store, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := task.MarshalCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	after, err := task.MarshalCanonical(operation.Request())
	if closeErr := operation.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(before) != string(after) {
		t.Fatalf("capture finalization renewed or changed helper authorization: %v", err)
	}
}
