package taskdir

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadOnlyOpenDoesNotAttemptFilesystemProbe(t *testing.T) {
	store := testStore(t)
	// A probe attempt fails even for a privileged test process. Checking only
	// the final tree would miss a probe that created and removed its files.
	probe := filepath.Join(store.Root, ".probe")
	must(t, os.Remove(probe))
	must(t, os.WriteFile(probe, []byte("read-only observation must not probe"), 0o600))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	if err != nil {
		t.Fatalf("passive open attempted a filesystem probe: %v", err)
	}
	t.Cleanup(func() { must(t, reader.Close()) })
	if reader.Root() != store.Root || reader.RootID() != store.RootID {
		t.Fatalf("reader changed the validated root binding")
	}
}

func TestReadOnlyEvidenceDoesNotPublishSealedResult(t *testing.T) {
	store := testStore(t)
	td := sealedTask(t, store, "durable answer")
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	evidence, err := reader.ReadTaskEvidence(t.Context(), td.TaskID)
	must(t, err)
	if !evidence.Exists || evidence.Started == nil || evidence.Seal == nil || evidence.Inspection.Publication != task.PublicationPending {
		t.Fatalf("sealed facts were lost: %+v", evidence)
	}
	if evidence.Inspection.Outcome != nil || evidence.OutcomeSHA256 != "" {
		t.Fatal("passive inspection published or fabricated a result")
	}
	if len(evidence.Logs) != 2 || !evidence.Logs[0].Sealed || !evidence.Logs[1].Sealed {
		t.Fatalf("sealed log descriptors missing: %+v", evidence.Logs)
	}
	if _, err = os.Lstat(filepath.Join(td.Dir, "outcome.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read published outcome: %v", err)
	}
}

func TestReadOnlyEvidenceRetainsCommittedOutcome(t *testing.T) {
	store := testStore(t)
	td := sealedTask(t, store, "durable answer")
	winner := collectOK(t, td)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	evidence, err := reader.ReadTaskEvidence(t.Context(), td.TaskID)
	must(t, err)
	if evidence.Inspection.Publication != task.PublicationCommitted || evidence.OutcomeSHA256 == "" || evidence.Inspection.Outcome.Payload != winner.Payload {
		t.Fatalf("validated result descriptor changed: %+v", evidence)
	}
	file, err := reader.OpenTaskPayload(t.Context(), td.TaskID, winner.Payload)
	must(t, err)
	data, readErr := io.ReadAll(file)
	if readErr != nil || string(data) != "durable answer" {
		t.Fatalf("payload=%q error=%v", data, readErr)
	}
	if _, writeErr := file.Write([]byte("unowned mutation")); writeErr == nil {
		t.Error("passive reader granted writable payload")
	}
	must(t, file.Close())
}

func TestReadOnlyEvidenceDistinguishesAbsenceAndPartialTask(t *testing.T) {
	store := testStore(t)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	const id = "11111111111111111111111111111111"
	evidence, err := reader.ReadTaskEvidence(t.Context(), id)
	must(t, err)
	if evidence.Exists {
		t.Fatal("absent task reported present")
	}
	must(t, os.MkdirAll(filepath.Join(store.Root, "tasks", id), 0o700))
	evidence, err = reader.ReadTaskEvidence(t.Context(), id)
	if err == nil || !evidence.Exists {
		t.Fatalf("partial task became absence: %+v, %v", evidence, err)
	}
}

func TestReadOnlyEvidenceRejectsIdentityFault(t *testing.T) {
	store := testStore(t)
	td := sealedTask(t, store, "durable answer")
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	evidence, err := reader.ReadTaskEvidence(t.Context(), td.TaskID)
	must(t, err)
	started := *evidence.Started
	started.TaskID = "11111111111111111111111111111111"
	data, err := marshalControlRecord(started)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(td.Dir, "provider.started.json"), data, 0o600))
	_, err = reader.ReadTaskEvidence(t.Context(), td.TaskID)
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("mismatched started record accepted: %v", err)
	}
}

func TestReadOnlyEvidenceHonorsContextAndClosedHandle(t *testing.T) {
	store := testStore(t)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ReadTaskEvidence(ctx, "11111111111111111111111111111111"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	must(t, reader.Close())
	if _, err = reader.ReadTaskEvidence(t.Context(), "11111111111111111111111111111111"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
	must(t, reader.Close())
}

func TestReadOnlyOpenDoesNotInitializeAbsentRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	_, err := OpenReadOnlyStoreContext(t.Context(), root, predicate.Default())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root: %v", err)
	}
	if _, err = os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read initialized missing root: %v", err)
	}
}
