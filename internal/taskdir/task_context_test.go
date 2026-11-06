package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ownContextTask(t *testing.T, fixture *storeOwnerFixture, td *TaskDir) {
	t.Helper()
	if td == nil {
		return
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
}

func contextPreparedTask(t *testing.T, fixture *storeOwnerFixture) (*TaskDir, *task.TaskRecord, *task.MetaRecord) {
	t.Helper()
	req, meta, brief := preparedInput(t, fixture.store)
	td, err := fixture.store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	ownContextTask(t, fixture, td)
	must(t, err)
	return td, req, meta
}

func TestTaskContextPortsRefuseCanceledMutation(t *testing.T) {
	fixture := newStoreOwnerFixture(t)
	td, req, meta := contextPreparedTask(t, fixture)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	permit, err := td.PrepareSubmissionContext(ctx, meta.SupervisorConfig)
	if permit != nil {
		t.Cleanup(func() { must(t, permit.Release()) })
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled submission: %v", err)
	}
	if _, err = td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled submission persisted: %v", err)
	}
	const conversation = "context-conversation"
	if err = td.ClaimSessionContext(ctx, req.Provider, conversation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claim: %v", err)
	}
	if _, err = os.Lstat(td.sessionDirectory(req.Provider, conversation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled claim initialized its journal: %v", err)
	}
	must(t, td.ClaimSessionContext(t.Context(), req.Provider, conversation))
	if err = td.ReleaseSessionContext(ctx, req.Provider, conversation, strings.Repeat("a", 64)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled release: %v", err)
	}
	outcome, cleanupErr, collectErr := td.CollectContext(ctx, meta.Predicate)
	if outcome != nil || cleanupErr != nil || !errors.Is(collectErr, context.Canceled) {
		t.Fatalf("canceled collection: cleanup=%v error=%v", cleanupErr, collectErr)
	}
	outcome, cleanupErr, collectErr = td.CollectContext(t.Context(), meta.Predicate)
	if outcome != nil || cleanupErr != nil || !errors.Is(collectErr, task.ErrNoSeal) {
		t.Fatalf("cold collection policy changed: cleanup=%v error=%v", cleanupErr, collectErr)
	}
}

type taskRecordCancellation struct {
	BaseFaultInjector
	cancel context.CancelFunc
}

func (f *taskRecordCancellation) OnBeforeStageCreate(_, destination string) error {
	if filepath.Base(destination) == "task.json" {
		f.cancel()
	}
	return nil
}

func TestCreateTaskContextCancellationKeepsRecoverableFragments(t *testing.T) {
	fixture := newStoreOwnerFixture(t)
	req, meta, brief := preparedInput(t, fixture.store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.store.SetFaultInjector(&taskRecordCancellation{cancel: cancel})
	td, err := fixture.store.CreateTaskContext(ctx, req.TaskID, req, brief, meta)
	ownContextTask(t, fixture, td)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation before task record was ignored: %v", err)
	}
	path := filepath.Join(fixture.store.Root, "tasks", req.TaskID)
	bytes, err := os.ReadFile(filepath.Join(path, "brief.md"))
	must(t, err)
	if string(bytes) != string(brief) {
		t.Fatal("accepted brief fragment changed")
	}
	if _, err = os.Lstat(filepath.Join(path, "task.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled record published: %v", err)
	}
	fixture.store.SetFaultInjector(nil)
	recovered, err := fixture.store.CreateTaskContext(t.Context(), req.TaskID, req, brief, meta)
	ownContextTask(t, fixture, recovered)
	must(t, err)
	actual, _, err := recovered.PreparedRecords()
	must(t, err)
	if actual.TaskID != req.TaskID || actual.BriefSHA256 != req.BriefSHA256 {
		t.Fatal("retry replaced accepted request")
	}
}
