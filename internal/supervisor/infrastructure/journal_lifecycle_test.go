package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func journalLifecycleBody(t *testing.T, j *intentJournal, kind epochLifecycleKind, sequence uint64) epochLifecycleIntent {
	t.Helper()
	body := epochLifecycleIntentFixture(t, kind)
	body.Sequence = sequence
	body.ID = task.ComputeSHA256([]byte(j.pin.Root() + string(kind) + string(rune(sequence))))[:32]
	body.Root, body.RootID, body.RootDevice, body.RootInode = j.pin.Root(), j.pin.RootID(), j.pin.Device(), j.pin.Inode()
	body.BootID, body.Owner = j.bootID, StartupReference{}
	body.AcceptedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	body.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if kind == epochLifecycleOwnerUpgrade {
		selected := contextForJournal(t, j)
		selected.OS = runtime.GOOS
		reference, _, err := j.stageContext(t.Context(), selected)
		if err != nil {
			t.Fatal(err)
		}
		body.SuccessorContext = reference
	}
	return body
}

func lifecycleReference(t *testing.T, body epochLifecycleIntent) epochLifecycleReference {
	t.Helper()
	reference, err := referenceEpochLifecycle(body)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func lifecycleProgress(t *testing.T, body epochLifecycleIntent, phase epochLifecyclePhase) epochLifecycleProgress {
	t.Helper()
	record := epochLifecycleProgress{Protocol: ProtocolVersion, OperationRef: lifecycleReference(t, body), Phase: phase, RecordedAt: body.AcceptedAt}
	if phase == epochLifecycleStopping {
		record.StopTarget = epochStopTarget{Kind: epochStopAbsent, BootID: body.BootID, Directory: epochNativeFileIdentity{Device: body.RootDevice, Inode: 41}, Bootstrap: epochNativeFileIdentity{Device: body.RootDevice, Inode: 42}}
	}
	return record
}

func requireLifecycleAccept(t *testing.T, j *intentJournal, lease *Lease, body epochLifecycleIntent) {
	t.Helper()
	if created, err := j.acceptLifecycle(t.Context(), lease, body); err != nil || !created {
		t.Fatalf("lifecycle acceptance created=%t err=%v", created, err)
	}
}

func requireLifecycleProgress(t *testing.T, j *intentJournal, lease *Lease, body epochLifecycleIntent, phase epochLifecyclePhase) {
	t.Helper()
	if created, err := j.appendLifecycleProgress(t.Context(), lease, lifecycleReference(t, body), lifecycleProgress(t, body, phase)); err != nil || !created {
		t.Fatalf("lifecycle progress %s created=%t err=%v", phase, created, err)
	}
}

func requireLifecycleBodyReplay(t *testing.T, j *intentJournal, lease *Lease, body epochLifecycleIntent) {
	t.Helper()
	if created, err := j.acceptLifecycle(t.Context(), lease, body); err != nil || created {
		t.Fatalf("equal body retry: %t %v", created, err)
	}
	changed := body
	changed.Deadline = time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	if created, err := j.acceptLifecycle(t.Context(), lease, changed); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting body: %t %v", created, err)
	}
}

func TestLifecycleJournalCreateOnceAndExactHistory(t *testing.T) {
	j, lease := journalFixture(t)
	body := journalLifecycleBody(t, j, epochLifecycleStop, 1)
	requireLifecycleAccept(t, j, lease, body)
	requireLifecycleBodyReplay(t, j, lease, body)
	requireLifecycleProgress(t, j, lease, body, epochLifecycleClosing)
	record := lifecycleProgress(t, body, epochLifecycleClosing)
	if created, err := j.appendLifecycleProgress(t.Context(), lease, lifecycleReference(t, body), record); err != nil || created {
		t.Fatalf("equal phase retry: %t %v", created, err)
	}
	record.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if created, err := j.appendLifecycleProgress(t.Context(), lease, lifecycleReference(t, body), record); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting phase: %t %v", created, err)
	}
	requireLifecycleProgress(t, j, lease, body, epochLifecycleStopping)
	requireLifecycleProgress(t, j, lease, body, epochLifecycleComplete)
	snapshot, found, err := j.latestLifecycle(t.Context())
	if err != nil || !found || snapshot.intent != body || snapshot.phase != epochLifecycleComplete || len(snapshot.records) != 3 {
		t.Fatalf("canonical history: found=%t phase=%s err=%v", found, snapshot.phase, err)
	}
	reference := lifecycleReference(t, body)
	referenced, err := j.readLifecycle(t.Context(), reference)
	if err != nil || referenced.intent != body || referenced.phase != epochLifecycleComplete {
		t.Fatalf("referenced body read changed history: %v", err)
	}
	reference.SHA256 = task.ComputeSHA256([]byte("wrong immutable body"))
	if _, err = j.readLifecycle(t.Context(), reference); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong body digest accepted: %v", err)
	}
}

func TestLifecycleJournalRejectsWrongOwnerLeaseAndOrdering(t *testing.T) {
	j, lease := journalFixture(t)
	body := journalLifecycleBody(t, j, epochLifecycleStop, 1)
	_, foreignLease := journalFixture(t)
	if created, err := j.acceptLifecycle(t.Context(), foreignLease, body); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign lease: %t %v", created, err)
	}
	body.Owner = StartupReference{Sequence: 1, Epoch: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("a", 64)}
	if created, err := j.acceptLifecycle(t.Context(), lease, body); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("invented logical owner: %t %v", created, err)
	}
	body.Owner = StartupReference{}
	requireLifecycleAccept(t, j, lease, body)
	if created, err := j.appendLifecycleProgress(t.Context(), lease, lifecycleReference(t, body), lifecycleProgress(t, body, epochLifecycleComplete)); created || err == nil {
		t.Fatalf("missing preceding phases: %t %v", created, err)
	}
	next := journalLifecycleBody(t, j, epochLifecycleStop, 2)
	if created, err := j.acceptLifecycle(t.Context(), lease, next); created || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("unresolved predecessor: %t %v", created, err)
	}
}

func TestLifecycleJournalResidentAcceptanceAndRetiredBodyFence(t *testing.T) {
	f := ownerJournal(t)
	requireAssumptionPut(t, f)
	if created, err := f.journal.putPublication(t.Context(), f.lease, f.publication); err != nil || !created {
		t.Fatal(err)
	}
	body := journalLifecycleBody(t, f.journal, epochLifecycleStop, 1)
	body.Owner = f.assumption.Startup
	requireLifecycleAccept(t, f.journal, f.lease, body)
	requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleClosing)
	requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleAborted)
	next := journalLifecycleBody(t, f.journal, epochLifecycleStop, 2)
	next.Owner = body.Owner
	requireLifecycleAccept(t, f.journal, f.lease, next)
	if created, err := f.journal.appendLifecycleProgress(t.Context(), f.lease, lifecycleReference(t, body), lifecycleProgress(t, body, epochLifecycleStopping)); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("old-body continuation escaped latest-body fence: %t %v", created, err)
	}
}

func TestLifecycleJournalMalformedAndOrphanEvidenceFaults(t *testing.T) {
	for _, fault := range []string{"orphan", "name", "gap"} {
		t.Run(fault, func(t *testing.T) {
			j, _ := journalFixture(t)
			body := journalLifecycleBody(t, j, epochLifecycleStop, 1)
			name := lifecycleName(body.ID)
			if fault == "name" {
				name = "lifecycle-invalid.json"
			}
			if fault == "gap" {
				body.Sequence = 2
			}
			err := j.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
				if fault == "orphan" {
					_, putErr := tx.Put(lifecycleProgressName(body.ID, epochLifecycleClosing), lifecycleProgress(t, body, epochLifecycleClosing))
					return putErr
				}
				_, putErr := tx.Put(name, body)
				return putErr
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = j.latestLifecycle(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("bad history accepted: %v", err)
			}
		})
	}
}

func requireLifecyclePassiveAbsence(t *testing.T) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, found, readErr := readLatestLifecycle(t.Context(), reader); readErr != nil || found {
		t.Fatalf("passive absence: %t %v", found, readErr)
	}
	if admissionErr := CheckPrivateTaskAdmissionContext(t.Context(), reader); admissionErr != nil {
		t.Fatalf("passive empty admission: %v", admissionErr)
	}
	if _, err = os.Lstat(filepath.Join(root, "infrastructure")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive read initialized journal: %v", err)
	}
}

func TestLifecycleJournalPassiveAbsenceAndHistoricalBoot(t *testing.T) {
	requireLifecyclePassiveAbsence(t)
	j, lease := journalFixture(t)
	body := journalLifecycleBody(t, j, epochLifecycleStop, 1)
	body.BootID = otherStableBoot(t, j.bootID)
	body.RootDevice, body.RootInode = 123, 456
	if err := j.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(lifecycleName(body.ID), body)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := j.latestLifecycle(t.Context())
	if err != nil || !found || snapshot.intent != body {
		t.Fatalf("historical observation reinterpreted boot/native coordinates: %v", err)
	}
	if created, writeErr := j.acceptLifecycle(t.Context(), lease, body); created || !errors.Is(writeErr, ErrIdentity) {
		t.Fatalf("old boot became current acceptance: %t %v", created, writeErr)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = j.latestLifecycle(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestLifecycleJournalNewBodyRequiresCurrentNativeBinding(t *testing.T) {
	for _, fault := range []string{"root-id", "device", "inode", "boot"} {
		t.Run(fault, func(t *testing.T) {
			j, lease := journalFixture(t)
			body := journalLifecycleBody(t, j, epochLifecycleStop, 1)
			switch fault {
			case "root-id":
				body.RootID = task.ComputeSHA256([]byte("other root"))[:32]
			case "device":
				body.RootDevice++
			case "inode":
				body.RootInode++
			case "boot":
				body.BootID = otherStableBoot(t, j.bootID)
			}
			if created, err := j.acceptLifecycle(t.Context(), lease, body); created || !errors.Is(err, ErrIdentity) {
				t.Fatalf("wrong current native binding: created=%t err=%v", created, err)
			}
			if _, found, err := j.latestLifecycle(t.Context()); err != nil || found {
				t.Fatalf("refused binding left a body: found=%t err=%v", found, err)
			}
		})
	}
}
