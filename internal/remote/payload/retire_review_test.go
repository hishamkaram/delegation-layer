package payload

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

type retirementBarrierFailure struct {
	taskdir.BaseFaultInjector
	target        string
	cause         error
	hits          int
	retry         bool
	directoryHits int
}

func (f *retirementBarrierFailure) OnDirBarrier(path string) error {
	if f.retry && path == filepath.Dir(f.target) {
		f.directoryHits++
		// The first journal open precedes the byte lease. Fail the second
		// open, whose barrier acknowledges the fence while holding EX.
		if f.directoryHits == 2 {
			f.hits++
			return f.cause
		}
	}
	return nil
}

func (f *retirementBarrierFailure) OnPostLinkDirBarrier(path string) error {
	if path == f.target {
		f.hits++
		return f.cause
	}
	return nil
}

func TestRetirementRetryRequiresDurableTombstoneBeforeDeleting(t *testing.T) {
	store, id := originRetirementFixture(t)
	fault := errors.New("tombstone directory barrier failed")
	hook := &retirementBarrierFailure{target: filepath.Join(store.Root, "remote-refs", id, "retirement.json"), cause: fault}
	store.SetFaultInjector(hook)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	for attempt := range 2 {
		hook.retry = attempt > 0
		report, err := Retire(t.Context(), store, id, remotedata.Origin, true)
		if !errors.Is(err, fault) || report.Applied || hook.hits != attempt+1 {
			t.Fatalf("attempt %d acknowledged an unbarriered fence: %+v %v hits=%d", attempt, report, err, hook.hits)
		}
		root, openErr := remotedata.Open(2, store.Root, store.RootID, id, remotedata.Snapshot, false)
		if openErr != nil {
			t.Fatalf("deleted before durable fence: %v", openErr)
		}
		if closeErr := root.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	store.SetFaultInjector(nil)
	if report, err := Retire(t.Context(), store, id, remotedata.Origin, true); err != nil || !report.Applied {
		t.Fatalf("durable retry: %+v %v", report, err)
	}
}

func TestOriginSnapshotSameTaskIDStillRegistersConsumer(t *testing.T) {
	owner, id := originRetirementFixture(t)
	_, _, template := lifecyclefixture.NewAppTestTask(t, false)
	request := *template
	request.TaskID = id
	path, err := remotedata.Path(2, owner.Root, owner.RootID, id, remotedata.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	request.CanonicalCwd = filepath.Join(path, "workspace")
	use, err := owner.BeginRemoteDataUse(t.Context(), request)
	if err != nil || use == nil {
		t.Fatalf("same-ID ordinary consumer lost its dependency: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := use.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = use.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = use.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Retire(t.Context(), owner, id, remotedata.Origin, true)
	if !errors.Is(err, ErrRetirementBlocked) || report.Applied || len(report.Blockers) != 1 {
		t.Fatalf("same-ID uncreated consumer did not fence retirement: %+v %v", report, err)
	}
}

func TestRetirementPreviewNeverOpensExecutionLock(t *testing.T) {
	store, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("e", 32)
	request.Remote = &task.RemoteBinding{OriginRootID: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64)}
	meta.TaskID, meta.SpecSHA256 = request.TaskID, ""
	td, err := store.CreateTask(request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// Read-only proof must not depend on opening .run.lock at all. The queued
	// task has no terminal closure, so apply cannot acquire it either.
	if err = os.Remove(filepath.Join(td.Dir, ".run.lock")); err != nil {
		t.Fatal(err)
	}
	reader := retirementReader(t, store)
	preview, err := PreviewRetirement(t.Context(), reader, request.TaskID, remotedata.Destination)
	if err != nil || preview.Retirable || len(preview.Blockers) != 1 || preview.Blockers[0] != "task_not_closed" {
		t.Fatalf("preview inspected execution lock: %+v %v", preview, err)
	}
	if report, applyErr := Retire(t.Context(), store, request.TaskID, remotedata.Destination, true); !errors.Is(applyErr, ErrRetirementBlocked) || report.Applied {
		t.Fatalf("unclosed apply inspected execution lock: %+v %v", report, applyErr)
	}
}
