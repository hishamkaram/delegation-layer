package payload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func originRetirementFixture(t *testing.T) (*taskdir.Store, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	store, _, _ := lifecyclefixture.NewAppTestTask(t, false)
	id := strings.Repeat("a", 32)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input"), []byte("frozen bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, snapshot, err := PrepareSnapshot(ctx, store, id, source, []string{"input"})
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	intent := remoteTransferIntent(t, store, store, id, manifest)
	reference, err := remote.OpenReference(ctx, store, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = reference.SaveIntent(ctx, intent, []byte("brief")); err != nil {
		t.Fatal(err)
	}
	if err = reference.Close(); err != nil {
		t.Fatal(err)
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.OpenRemoteReferenceContext(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.PutContext(ctx, "send-intent.json", remote.SendIntent{IntentSHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	return store, id
}

func TestRemoteOriginPreviewPreservesSnapshotAndUploadLease(t *testing.T) {
	store, id := originRetirementFixture(t)
	reader := retirementReader(t, store)
	usage, err := Usage(t.Context(), reader, id, remotedata.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if usage.LayoutVersion != 2 || len(usage.Families) != 1 || usage.Families[0].Bytes < 12 {
		t.Fatalf("usage=%+v", usage)
	}
	preview, err := PreviewRetirement(t.Context(), reader, id, remotedata.Origin)
	if err != nil || !preview.Retirable || preview.Applied {
		t.Fatalf("preview=%+v %v", preview, err)
	}
	_, snapshot, err := LoadSnapshot(t.Context(), store, id)
	if err != nil {
		t.Fatal("preview removed bytes", err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireRemoteByteUse(t.Context(), id, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Retire(t.Context(), store, id, remotedata.Origin, true); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("retire during upload=%v", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteOriginRetirementIsExplicitIdempotentAndKeepsHistory(t *testing.T) {
	store, id := originRetirementFixture(t)
	reader := retirementReader(t, store)
	report, err := Retire(t.Context(), store, id, remotedata.Origin, true)
	if err != nil || !report.Applied {
		t.Fatalf("apply=%+v %v", report, err)
	}
	again, err := Retire(t.Context(), store, id, remotedata.Origin, true)
	if err != nil || !again.AlreadyDone {
		t.Fatalf("repeat=%+v %v", again, err)
	}
	if _, _, err = LoadSnapshot(t.Context(), store, id); !errors.Is(err, ErrRetired) {
		t.Fatalf("retired snapshot loaded %v", err)
	}
	usage, err := Usage(t.Context(), reader, id, remotedata.Origin)
	if err != nil || !usage.Retired || usage.Families[0].Availability != "retired" {
		t.Fatalf("retired usage=%+v %v", usage, err)
	}
	reference, err := remote.OpenReference(t.Context(), store, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reference.Intent(); err != nil {
		t.Fatal("history removed", err)
	}
	if err = reference.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteOriginRetirementBlocksUncreatedCrossRootConsumer(t *testing.T) {
	owner, id := originRetirementFixture(t)
	consumer, _, original := lifecyclefixture.NewAppTestTask(t, false)
	req := *original
	req.TaskID = strings.Repeat("b", 32)
	path, err := remotedata.Path(2, owner.Root, owner.RootID, id, remotedata.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	req.CanonicalCwd = filepath.Join(path, "workspace")
	use, err := consumer.BeginRemoteDataUse(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err = use.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = use.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Retire(t.Context(), owner, id, remotedata.Origin, true)
	if !errors.Is(err, ErrRetirementBlocked) || report.Applied || len(report.Blockers) != 1 {
		t.Fatalf("unknown consumer=%+v %v", report, err)
	}
	_, snapshot, err := LoadSnapshot(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func completeRetirementTask(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := permit.Release(); e != nil {
			t.Error(e)
		}
	})
	for _, operation := range []func() error{permit.Consume, func() error { return td.RecordStarted(0) }, func() error { return td.WriteRawFiles("answer\n", "") }} {
		if err = operation(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	seedRemoteCaptureClosure(t, td)
	if outcome, cleanup, e := td.Collect(task.FixturePredicateRef()); e != nil || cleanup != nil || outcome == nil {
		t.Fatalf("fixture did not finish: outcome=%+v error=%v", outcome, errors.Join(e, cleanup))
	}
}

func TestRemoteDestinationRetirementKeepsWorkspaceAndReceipts(t *testing.T) {
	store, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req := *original
	req.TaskID = strings.Repeat("d", 32)
	req.Remote = &task.RemoteBinding{StorageLayout: task.RemoteDataOrganized, OriginRootID: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64), OutputPaths: []string{"result.txt"}}
	meta.TaskID, meta.SpecSHA256 = req.TaskID, ""
	td, err := store.CreateTask(req.TaskID, &req, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := td.Close(); e != nil {
			t.Error(e)
		}
	})
	if err = os.WriteFile(filepath.Join(req.CanonicalCwd, "result.txt"), []byte("selected result"), 0o600); err != nil {
		t.Fatal(err)
	}
	completeRetirementTask(t, td)
	if report, e := Retire(t.Context(), store, req.TaskID, remotedata.Destination, true); !errors.Is(e, ErrRetirementBlocked) || !reflect.DeepEqual(report.Blockers, []string{"capture_pending"}) {
		t.Fatalf("uncaptured output=%+v %v", report, e)
	}
	receipt, err := CaptureOutputs(t.Context(), store, td)
	if err != nil {
		t.Fatal(err)
	}
	if report, e := Retire(t.Context(), store, req.TaskID, remotedata.Destination, true); e != nil || !report.Applied {
		t.Fatalf("retire destination=%+v %v", report, e)
	}
	if data, e := os.ReadFile(filepath.Join(req.CanonicalCwd, "result.txt")); e != nil || string(data) != "selected result" {
		t.Fatalf("prepared cwd removed: %q %v", data, e)
	}
	assertRetirementFamiliesRemoved(t, store, req.TaskID)
	repeated, err := CaptureOutputs(t.Context(), store, td)
	if err != nil || !reflect.DeepEqual(receipt, repeated) {
		t.Fatalf("retired capture changed: %+v %v", repeated, err)
	}
	assertRetiredOutputDownload(t, store, &req)
}

func TestRemoteDestinationRetirementBlocksLegacyInputConsumers(t *testing.T) {
	store, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req := *original
	req.TaskID = strings.Repeat("e", 32)
	req.Remote = &task.RemoteBinding{OriginRootID: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64), InputSHA256: strings.Repeat("d", 64)}
	meta.TaskID, meta.SpecSHA256 = req.TaskID, ""
	td, err := store.CreateTask(req.TaskID, &req, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := td.Close(); e != nil {
			t.Error(e)
		}
	})
	completeRetirementTask(t, td)
	report, err := Retire(t.Context(), store, req.TaskID, remotedata.Destination, true)
	if !errors.Is(err, ErrRetirementBlocked) || !reflect.DeepEqual(report.Blockers, []string{"legacy_inputs_consumers_unknown"}) {
		t.Fatalf("legacy consumer=%+v %v", report, err)
	}
}

type retirementInterruption struct {
	taskdir.BaseFaultInjector
	target  string
	failure error
	fired   bool
}

func (f *retirementInterruption) OnAfterLinkDirBarrier(path string) error {
	if path == f.target && !f.fired {
		f.fired = true
		return f.failure
	}
	return nil
}

func TestRemoteOriginRetirementResumesAfterDurableTombstone(t *testing.T) {
	store, id := originRetirementFixture(t)
	injected := errors.New("interruption after tombstone publication")
	hook := &retirementInterruption{target: filepath.Join(store.Root, "remote-refs", id, "retirement.json"), failure: injected}
	store.SetFaultInjector(hook)
	if report, err := Retire(t.Context(), store, id, remotedata.Origin, true); !hook.fired || !errors.Is(err, injected) || report.Applied {
		t.Fatalf("interruption=%+v %v fired=%v", report, err, hook.fired)
	}
	store.SetFaultInjector(nil)
	reader := retirementReader(t, store)
	before, present, err := ReadRetirement(t.Context(), reader, id, remotedata.Origin)
	if err != nil || !present {
		t.Fatalf("tombstone missing: %+v %v", before, err)
	}
	if _, _, err = LoadSnapshot(t.Context(), store, id); !errors.Is(err, ErrRetired) {
		t.Fatalf("interrupted retirement admitted new use: %v", err)
	}
	if report, e := Retire(t.Context(), store, id, remotedata.Origin, true); e != nil || !report.Applied || report.AlreadyDone {
		t.Fatalf("resume=%+v %v", report, e)
	}
	after, present, err := ReadRetirement(t.Context(), reader, id, remotedata.Origin)
	if err != nil || !present || !reflect.DeepEqual(before, after) {
		t.Fatalf("tombstone changed: before=%+v after=%+v %v", before, after, err)
	}
}

func TestRemoteOriginRetirementWaitsForCanonicalConsumerClosure(t *testing.T) {
	owner, id := originRetirementFixture(t)
	consumer, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req := *original
	req.TaskID = strings.Repeat("b", 32)
	path, err := remotedata.Path(2, owner.Root, owner.RootID, id, remotedata.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	req.CanonicalCwd = filepath.Join(path, "workspace")
	use, err := consumer.BeginRemoteDataUse(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := use.Close(); e != nil {
			t.Error(e)
		}
	})
	if err = use.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	meta.TaskID, meta.SpecSHA256 = req.TaskID, ""
	td, err := consumer.CreateTask(req.TaskID, &req, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := td.Close(); e != nil {
			t.Error(e)
		}
	})
	if err = use.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Retire(t.Context(), owner, id, remotedata.Origin, true)
	if !errors.Is(err, ErrRetirementBlocked) || len(report.Blockers) != 1 {
		t.Fatalf("live consumer=%+v %v", report, err)
	}
	completeRetirementTask(t, td)
	report, err = Retire(t.Context(), owner, id, remotedata.Origin, true)
	if err != nil || !report.Applied {
		t.Fatalf("closed consumer=%+v %v", report, err)
	}
}

func retirementReader(t *testing.T, store *taskdir.Store) *taskdir.ReadOnlyStore {
	t.Helper()
	reader, err := store.RemoteReader(t.Context(), store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := reader.Close(); e != nil {
			t.Error(e)
		}
	})
	return reader
}

func assertRetirementFamiliesRemoved(t *testing.T, store *taskdir.Store, id string) {
	t.Helper()
	for _, family := range []remotedata.Family{remotedata.Inputs, remotedata.Outputs} {
		root, err := remotedata.Open(2, store.Root, store.RootID, id, family, false)
		if root != nil {
			if e := root.Close(); e != nil {
				t.Error(e)
			}
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned family remains: %s %v", family, err)
		}
	}
}

func assertRetiredOutputDownload(t *testing.T, store *taskdir.Store, req *task.TaskRecord) {
	t.Helper()
	reader := retirementReader(t, store)
	response := Download(t.Context(), reader, remote.Request{Operation: remote.Download, Root: store.Root, TaskID: req.TaskID, IntentSHA256: req.Remote.IntentSHA256, Expected: &remote.Identity{RootID: store.RootID}, Transfer: &remote.TransferRequest{Selector: "output:result.txt"}}, remote.Response{})
	if response.Failure != nil || response.Transfer == nil || response.Transfer.Availability != "retired" {
		t.Fatalf("retired download=%+v", response)
	}
}
