package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newRemoteCaptureTask(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, seed, original := newAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("a", 32)
	request.Remote = &task.RemoteBinding{OriginRootID: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64), OutputPaths: []string{"missing", "result.txt"}}
	metadata := *meta
	metadata.TaskID = request.TaskID
	metadata.SpecSHA256 = ""
	td, err := store.CreateTask(request.TaskID, &request, []byte("brief"), &metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, td, &request
}

func TestRemoteCaptureRequiresCanonicalClosureBeforeWriting(t *testing.T) {
	store, td, request := newRemoteCaptureTask(t)
	if _, err := captureRemoteOutputs(t.Context(), store, td); !errors.Is(err, guardian.ErrProofPending) {
		t.Fatalf("capture without closure: %v", err)
	}
	if _, err := store.OpenRemoteCaptureContext(t.Context(), request.TaskID, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture state created before closure: %v", err)
	}
}

func TestRemoteCapturePersistsSelectedBytesAndReadsWithoutSource(t *testing.T) {
	store, td, request := newRemoteCaptureTask(t)
	if err := os.WriteFile(filepath.Join(request.CanonicalCwd, "result.txt"), []byte("sealed output"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedRemoteCaptureClosure(t, td)
	receipt, err := captureRemoteOutputs(t.Context(), store, td)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Files) != 2 || receipt.Files[0].Status != "absent" || receipt.Files[1].Status != "captured" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if err = os.Remove(filepath.Join(request.CanonicalCwd, "result.txt")); err != nil {
		t.Fatal(err)
	}
	saved, err := readRemoteCapture(t.Context(), store, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CapturedAt != receipt.CapturedAt || saved.Files[1].Entry.SHA256 != receipt.Files[1].Entry.SHA256 {
		t.Fatalf("changed receipt=%+v", saved)
	}
	repeated, err := captureRemoteOutputs(t.Context(), store, td)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Files[1].Status != "captured" {
		t.Fatal("recovery reread mutable source")
	}
}

func TestRemoteCaptureDirectoryRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "state")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, ".state.remote-outputs")); err != nil {
		t.Fatal(err)
	}
	if _, err := openRemoteCaptureDirectory(root, strings.Repeat("a", 32), strings.Repeat("b", 32), true); err == nil {
		t.Fatal("symlink capture directory accepted")
	}
}

func seedRemoteCaptureClosure(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	facts := taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "remote-capture-fixture", BootID: "remote-fixture-boot", Deadline: time.Now().Add(time.Minute).UTC()}
	if _, err = td.RecordLifetimeIntentContext(t.Context(), facts); err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID); err != nil {
		t.Fatal(err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteCaptureRejectsUnsafeFileAndContinues(t *testing.T) {
	store, td, request := newRemoteCaptureTask(t)
	if err := os.Symlink("result.txt", filepath.Join(request.CanonicalCwd, "missing")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(request.CanonicalCwd, "result.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedRemoteCaptureClosure(t, td)
	receipt, err := captureRemoteOutputs(t.Context(), store, td)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Files[0].Status != "rejected" || receipt.Files[1].Status != "captured" {
		t.Fatalf("capture blocked later file: %+v", receipt)
	}
	if receipt.Files[1].CapturedAt == "" || receipt.CompletedAt == "" {
		t.Fatal("actual capture timing missing")
	}
	if err = os.RemoveAll(request.CanonicalCwd); err != nil {
		t.Fatal(err)
	}
	repeated, err := captureRemoteOutputs(t.Context(), store, td)
	if err != nil || repeated.CompletedAt != receipt.CompletedAt {
		t.Fatalf("complete capture reread deleted cwd: %+v %v", repeated, err)
	}
}

func TestRemoteCaptureDeadlinePublishesTerminalDisposition(t *testing.T) {
	store, _, request := newRemoteCaptureTask(t)
	journal, err := store.OpenRemoteCaptureContext(t.Context(), request.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	var receipt remotefiles.Receipt
	err = journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		return captureRemoteTransaction(expired, store, request, tx, &receipt)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range receipt.Files {
		if file.Status != "incomplete" || file.Reason != "capture_deadline" {
			t.Fatalf("deadline not terminal: %+v", file)
		}
	}
	var complete remoteCaptureStart
	if err = journal.Read("complete.json", &complete); err != nil || complete.CompletedAt == "" {
		t.Fatalf("completion not durable: %+v %v", complete, err)
	}
}

func TestRemoteCaptureRecoveryDoesNotRenewDeadline(t *testing.T) {
	store, _, request := newRemoteCaptureTask(t)
	if err := os.WriteFile(filepath.Join(request.CanonicalCwd, "result.txt"), []byte("must not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := store.OpenRemoteCaptureContext(t.Context(), request.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	began := time.Now().Add(-time.Hour).UTC()
	start := remoteCaptureStart{Provenance: remotefiles.Provenance{OriginRootID: request.Remote.OriginRootID, RootID: request.RootID, TaskID: request.TaskID}, CapturedAt: began.Format(time.RFC3339Nano), Deadline: began.Add(remoteCaptureBudget).Format(time.RFC3339Nano), Count: len(request.Remote.OutputPaths)}
	if _, err = journal.PutContext(t.Context(), "started.json", start); err != nil {
		t.Fatal(err)
	}
	var receipt remotefiles.Receipt
	err = journal.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		return captureRemoteTransaction(t.Context(), store, request, tx, &receipt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Files[1].Status != "incomplete" || receipt.Files[1].Entry != nil {
		t.Fatalf("recovery renewed capture budget: %+v", receipt)
	}
}

func TestRemoteCaptureDestinationUnavailableIsIncomplete(t *testing.T) {
	store, td, request := newRemoteCaptureTask(t)
	if err := os.WriteFile(filepath.Join(request.CanonicalCwd, "result.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(filepath.Dir(store.Root), "."+filepath.Base(store.Root)+".remote-outputs")); err != nil {
		t.Fatal(err)
	}
	seedRemoteCaptureClosure(t, td)
	receipt, err := captureRemoteOutputs(t.Context(), store, td)
	if err != nil {
		t.Fatal(err)
	}
	if file := receipt.Files[1]; file.Status != "incomplete" || file.Reason != "destination_unavailable" || file.Entry != nil {
		t.Fatalf("destination fault rejected source: %+v", file)
	}
}
