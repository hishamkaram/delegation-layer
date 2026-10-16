package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

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

func prepareClosedVerificationCapture(t *testing.T) (*workspaceRecoveryFixture, workspace.OperationRequest, workspace.OperationRequest, Dependencies) {
	t.Helper()
	fixture := newWorkspaceRecoveryFixture(t)
	verification, subject, deps := workspaceVerificationRecoveryAuthorization(t, fixture)
	operation, err := workspace.LoadOperation(t.Context(), fixture.store, verification.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := fixture.resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = recordWorkspaceVerificationUse(t.Context(), fixture.resource, reservation, operation); err != nil {
		t.Fatal(err)
	}
	seedRealWorkspaceOperationClosure(t, fixture.store, operation)
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	capture := openRecoveryOperation(t, fixture, strings.Repeat("f", 32), workspace.OperationCapture, subject)
	if _, err = fixture.resource.AssociateOperation(t.Context(), workspace.CapturePurpose(reservation.Sequence), capture.Request()); err != nil {
		t.Fatal(err)
	}
	seedRealWorkspaceOperationClosure(t, fixture.store, capture)
	recordRecoveredCaptureCandidate(t, fixture, capture)
	request := capture.Request()
	if err = capture.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)
	workspaceRecoveryUseOwner(t, fixture)
	return fixture, verification, request, deps
}

func TestWorkspaceClosedVerificationCaptureSurvivesObserverCancellationDuringReplay(t *testing.T) {
	fixture, verification, capture, deps := prepareClosedVerificationCapture(t)
	observer, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &workspaceReplayObserverCancellation{
		path: filepath.Join(fixture.store.Root, "workspace-operations", capture.OperationID, ".operation.lock"), cancel: cancel,
	}
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() { fixture.store.SetFaultInjector(nil) })
	var commands atomic.Int32
	deps.SupervisorOptions.Observer = func(pueue.CommandEvent) { commands.Add(1) }
	err := reconcileWorkspaceUse(observer, fixture.store, fixture.resource, deps)
	if !fault.fired || observer.Err() != context.Canceled || err != nil {
		t.Fatalf("closed capture replay followed the observer cancellation: fired=%t observer=%v reconcile=%v", fault.fired, observer.Err(), err)
	}
	receipt, err := fixture.resource.CaptureReceipt(t.Context(), capture.OperationID)
	if err != nil || receipt.Status != "captured" || receipt.Subject != capture.Subject {
		t.Fatalf("closed verification capture was not sealed: %+v, %v", receipt, err)
	}
	if _, err = fixture.resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("sealed capture retained its usage reservation: %v", err)
	}
	if commands.Load() != 0 {
		t.Fatalf("closed capture replay contacted the supervisor %d times", commands.Load())
	}
	assertWorkspaceReplayRequestUnchanged(t, fixture.store, verification)
	assertWorkspaceReplayRequestUnchanged(t, fixture.store, capture)
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

func TestWorkspaceControlTransactionNestedCallsRetainOneCutoff(t *testing.T) {
	observer, cancelObserver := context.WithCancel(t.Context())
	cancelObserver()
	control, finish := workspaceSchedulingControl(observer, Dependencies{})
	defer finish()
	cutoff, finite := control.Deadline()
	if !finite || control.Err() != nil {
		t.Fatal("coordinator inherited an expired observer instead of its finite control cutoff")
	}
	nested, closeNested := workspaceSchedulingControl(control, Dependencies{SupervisorOptions: pueue.Options{ObservationTimeout: time.Hour}})
	defer closeNested()
	nestedCutoff, finite := nested.Deadline()
	if !finite || !nestedCutoff.Equal(cutoff) {
		t.Fatal("nested scheduling renewed the control transaction")
	}
	finish()
	if nested.Err() != context.Canceled {
		t.Fatal("nested scheduling detached from its control owner")
	}
	replayed, closeReplay := workspaceSchedulingControl(control, Dependencies{})
	defer closeReplay()
	replayedCutoff, finite := replayed.Deadline()
	if !finite || !replayedCutoff.Equal(cutoff) || replayed.Err() != context.Canceled {
		t.Fatal("nested replay revived an already-canceled control transaction")
	}
}

func TestWorkspaceControlTransactionNestedCallsKeepEarlierHelperDeadline(t *testing.T) {
	control, finish := workspaceSchedulingControl(t.Context(), Dependencies{})
	defer finish()
	cutoff, _ := control.Deadline()
	helperDeadline := cutoff.Add(-time.Second)
	helper, closeHelper := context.WithDeadline(control, helperDeadline)
	defer closeHelper()
	nested, closeNested := workspaceSchedulingControl(helper, Dependencies{})
	defer closeNested()
	deadline, finite := nested.Deadline()
	if !finite || !deadline.Equal(helperDeadline) {
		t.Fatal("nested scheduling escaped the original helper deadline")
	}
}
