package workspaces

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func newTaskCaptureContextFixture(t *testing.T) (*taskdir.Store, *workspace.Resource, task.TaskRecord) {
	t.Helper()
	store, resource := workspacefixture.NewResource(t)
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	owner, err := resource.AcquireUseOwner(t.Context())
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := resource.Request()
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: admission.TaskID, SHA256: admission.RequestSHA256})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: admission.TaskID,
		Provider: "fixture", Mode: "write", CanonicalCwd: admission.SourceCwd,
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256([]byte("capture request")),
		Workspace: &task.WorkspaceBinding{ID: admission.WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: admission.RequestSHA256, BudgetNanos: int64(time.Minute)},
	}
	return store, resource, request
}

func TestRecordTaskCaptureContextCanceledBeforeWrite(t *testing.T) {
	store, resource, request := newTaskCaptureContextFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RecordTaskCaptureContext(ctx, store, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled capture = %v, want caller cancellation", err)
	}
	if _, err := resource.CaptureObligation(t.Context(), request.Workspace.Reservation); !workspace.OperationAbsent(err) {
		t.Fatalf("canceled capture changed obligation: %v", err)
	}
}

func TestRecordTaskCaptureContextCompletesAcceptedWrite(t *testing.T) {
	store, resource, request := newTaskCaptureContextFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observer := &captureStageCancellation{cancel: cancel}
	store.SetFaultInjector(observer)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	if err := RecordTaskCaptureContext(ctx, store, request); err != nil {
		t.Fatalf("accepted capture staging interrupted: %v", err)
	}
	if !observer.staged || !observer.barrier || !observer.cleaned || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("accepted write did not complete after cancellation: %+v, %v", observer, ctx.Err())
	}
	store.SetFaultInjector(nil)
	requireTaskCaptureObligation(t, resource, request)
}

func TestRecordTaskCaptureContextPreservesReplayAndConflict(t *testing.T) {
	store, resource, request := newTaskCaptureContextFixture(t)
	if err := RecordTaskCaptureContext(t.Context(), store, request); err != nil {
		t.Fatal(err)
	}
	if err := RecordTaskCapture(store, request); err != nil {
		t.Fatalf("legacy wrapper lost canonical replay: %v", err)
	}
	conflicting := request
	conflicting.BriefSHA256 = task.ComputeSHA256([]byte("other capture request"))
	if err := RecordTaskCaptureContext(t.Context(), store, conflicting); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting saved request = %v", err)
	}
	requireTaskCaptureObligation(t, resource, request)
}

func requireTaskCaptureObligation(t *testing.T, resource *workspace.Resource, request task.TaskRecord) {
	t.Helper()
	subject, err := TaskSubject(request)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := resource.CaptureObligation(t.Context(), request.Workspace.Reservation)
	if err != nil || obligation.Subject != subject || obligation.Predecessor == nil || *obligation.Predecessor != subject || obligation.BudgetNanos != request.Workspace.BudgetNanos {
		t.Fatalf("canonical task capture obligation = %+v, %v", obligation, err)
	}
}

type captureStageCancellation struct {
	taskdir.BaseFaultInjector
	cancel  context.CancelFunc
	staged  bool
	barrier bool
	cleaned bool
}

func (f *captureStageCancellation) OnStageWrite(path string, data []byte) (int, error) {
	if strings.HasPrefix(filepath.Base(path), "capture-obligation-") {
		f.staged = true
		f.cancel()
	}
	return len(data), nil
}

func (f *captureStageCancellation) OnStageBarrier(path string) error {
	if strings.HasPrefix(filepath.Base(path), "capture-obligation-") {
		f.barrier = true
	}
	return nil
}

func (f *captureStageCancellation) OnCleanupDirBarrier(string) error {
	if f.staged {
		f.cleaned = true
	}
	return nil
}
