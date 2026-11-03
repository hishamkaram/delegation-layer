package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type ownerJournalFixture struct {
	journal     *intentJournal
	lease       *Lease
	intent      StartupIntent
	assumption  StartupAssumption
	publication OwnerPublication
}

func ownerJournal(t *testing.T) ownerJournalFixture {
	t.Helper()
	journal, lease := journalFixture(t)
	intent := liveJournalIntent(t, journal)
	requireJournalPut(t, journal, lease, intent)
	assumption, publication := evidenceForIntent(t, intent)
	return ownerJournalFixture{journal, lease, intent, assumption, publication}
}

func liveJournalIntent(t *testing.T, journal *intentJournal) StartupIntent {
	t.Helper()
	intent := journalIntent(t, journal, 1)
	now := time.Now().UTC()
	intent.AcceptedAt = now.Add(-time.Second).Format(time.RFC3339Nano)
	intent.Deadline = now.Add(time.Hour).Format(time.RFC3339Nano)
	return intent
}

func evidenceForIntent(t *testing.T, intent StartupIntent) (StartupAssumption, OwnerPublication) {
	t.Helper()
	reference, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	assumption := StartupAssumption{Startup: reference, Process: ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}, UID: os.Geteuid(), AssumedAt: intent.AcceptedAt}
	digest, err := assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication := OwnerPublication{
		Protocol: ProtocolVersion, Startup: reference, AssumptionSHA256: digest,
		RuntimeSHA256: intent.Inputs.Artifacts.RunnerSHA256,
		Endpoint: EndpointIdentity{
			Path:         filepath.Join(intent.Inputs.Root, ".i"),
			DirectoryDev: intent.Inputs.RootDevice, DirectoryInode: intent.Inputs.RootInode, SocketInode: 24,
		},
	}
	return assumption, publication
}

func requireAssumptionPut(t *testing.T, fixture ownerJournalFixture) {
	t.Helper()
	created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, fixture.assumption)
	if err != nil || !created {
		t.Fatalf("assumption publication: %v %v", created, err)
	}
}

func TestOwnerJournalCreatesAssumptionAndPublicationOnce(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, fixture.assumption)
	if err != nil || created {
		t.Fatalf("duplicate assumption granted fresh creation: %v %v", created, err)
	}
	created, err = fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication)
	if err != nil || !created {
		t.Fatalf("publication: %v %v", created, err)
	}
	created, err = fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication)
	if err != nil || created {
		t.Fatalf("duplicate publication granted fresh creation: %v %v", created, err)
	}
	assumption, err := fixture.journal.readAssumption(t.Context(), fixture.assumption.Startup)
	if err != nil || assumption != fixture.assumption {
		t.Fatalf("assumption read: %v", err)
	}
	publication, err := fixture.journal.readPublication(t.Context(), fixture.publication.Startup)
	if err != nil || publication != fixture.publication {
		t.Fatalf("publication read: %v", err)
	}
}

func TestOwnerJournalAssumptionChecksOriginalDeadlineInTransaction(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			checkOwnerJournalDeadlineBoundary(t, expired)
		})
	}
}

func TestOwnerJournalConcurrentAssumptionHasOneWinner(t *testing.T) {
	fixture := ownerJournal(t)
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	ctx := t.Context()
	for range 2 {
		go func() {
			<-start
			created, err := fixture.journal.putAssumption(ctx, fixture.lease, fixture.assumption)
			results <- result{created, err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Error(result.err)
		}
		if result.created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("created winners = %d, want one", winners)
	}
}

func TestOwnerJournalRejectsWrongLeaseAndReferences(t *testing.T) {
	fixture := ownerJournal(t)
	_, otherLease := journalFixture(t)
	if created, err := fixture.journal.putAssumption(t.Context(), otherLease, fixture.assumption); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign lease accepted: %v %v", created, err)
	}
	wrong := fixture.assumption
	wrong.Startup.IntentSHA256 = task.ComputeSHA256([]byte("different intent"))
	if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, wrong); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed intent reference accepted: %v %v", created, err)
	}
	requireAssumptionPut(t, fixture)
	publication := fixture.publication
	publication.AssumptionSHA256 = task.ComputeSHA256([]byte("different assumption"))
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, publication); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed assumption digest accepted: %v %v", created, err)
	}
	if err := fixture.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication); created || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed lease accepted: %v %v", created, err)
	}
}

func writeOwnerEvidenceFixture[T StartupAssumption | OwnerPublication](t *testing.T, journal *intentJournal, name string, record T) {
	t.Helper()
	err := journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		created, err := tx.Put(name, record)
		if err == nil && !created {
			return errors.New("fixture record was not created")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOwnerJournalCurrentWritesRejectHistoricalBootAndNativePin(t *testing.T) {
	for _, changed := range []string{"boot", "device", "inode", "root"} {
		t.Run(changed, func(t *testing.T) { checkOwnerJournalStaleBinding(t, changed) })
	}
}

func checkOwnerJournalStaleBinding(t *testing.T, changed string) {
	t.Helper()
	journal, lease := journalFixture(t)
	intent := liveJournalIntent(t, journal)
	switch changed {
	case "boot":
		intent.Inputs.BootID = otherStableBoot(t, journal.bootID)
	case "device":
		intent.Inputs.RootDevice++
	case "inode":
		intent.Inputs.RootInode++
	case "root":
		intent.Inputs.Root = testOrigin(t).Root()
		intent.Starter.Root = intent.Inputs.Root
	}
	refreshIntentDigest(t, &intent)
	putHistoricalFixture(t, journal, intent)
	assumption, publication := evidenceForIntent(t, intent)
	if created, err := journal.putAssumption(t.Context(), lease, assumption); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("stale %s assumption: %v %v", changed, created, err)
	}
	writeOwnerEvidenceFixture(t, journal, assumptionName(1), assumption)
	if created, err := journal.putPublication(t.Context(), lease, publication); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("stale %s publication: %v %v", changed, created, err)
	}
}

func TestOwnerJournalHistoricalReadsDoNotReinterpretBoot(t *testing.T) {
	journal, _ := journalFixture(t)
	intent := liveJournalIntent(t, journal)
	intent.Inputs.BootID = otherStableBoot(t, journal.bootID)
	intent.Inputs.RootDevice++
	intent.Inputs.RootInode++
	refreshIntentDigest(t, &intent)
	putHistoricalFixture(t, journal, intent)
	assumption, publication := evidenceForIntent(t, intent)
	writeOwnerEvidenceFixture(t, journal, assumptionName(1), assumption)
	writeOwnerEvidenceFixture(t, journal, publicationName(1), publication)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	savedAssumption, err := readStartupAssumption(t.Context(), reader, assumption.Startup)
	if err != nil || savedAssumption != assumption {
		t.Fatalf("passive historical assumption: %v", err)
	}
	savedPublication, err := readOwnerPublication(t.Context(), reader, publication.Startup)
	if err != nil || savedPublication != publication {
		t.Fatalf("passive historical publication: %v", err)
	}
}

func TestOwnerJournalRejectsTamperedPublicationChain(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication)
	if err != nil || !created {
		t.Fatalf("publication: %v %v", created, err)
	}
	changed := fixture.assumption
	changed.UID++
	data, err := task.MarshalCanonical(changed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.journal.pin.Root(), "infrastructure", assumptionName(1))
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.journal.readPublication(t.Context(), fixture.publication.Startup); !errors.Is(err, ErrIdentity) {
		t.Fatalf("tampered assumption chain accepted: %v", err)
	}
}

func TestOwnerJournalCancelledAndClosedRefuse(t *testing.T) {
	fixture := ownerJournal(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if created, err := fixture.journal.putAssumption(ctx, fixture.lease, fixture.assumption); created || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled assumption: %v %v", created, err)
	}
	if err := fixture.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication); created || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed publication: %v %v", created, err)
	}
}

func checkOwnerJournalDeadlineBoundary(t *testing.T, expired bool) {
	t.Helper()
	fixture := ownerJournal(t)
	deadline, err := canonicalStartupTime(fixture.intent.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	competing, err := taskdir.OpenLockFile(filepath.Join(fixture.journal.pin.Root(), "infrastructure", ".operation.lock"), taskdir.LockLevelAdmission)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := competing.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	checked := false
	clock := func() time.Time {
		checked = true
		if lockErr := competing.LockEXNonblocking(); !errors.Is(lockErr, task.ErrLockBusy) {
			t.Errorf("deadline checked outside canonical transaction: %v", lockErr)
		}
		if expired {
			return deadline
		}
		return deadline.Add(-time.Nanosecond)
	}
	created, err := fixture.journal.putAssumptionWithClock(t.Context(), fixture.lease, fixture.assumption, clock)
	if !checked {
		t.Fatal("transaction did not evaluate original deadline")
	}
	if expired {
		if created || !errors.Is(err, ErrIdentity) {
			t.Fatalf("deadline equality accepted: %v %v", created, err)
		}
		if _, readErr := fixture.journal.readAssumption(t.Context(), fixture.assumption.Startup); !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("expired refusal wrote record: %v", readErr)
		}
	} else if err != nil || !created {
		t.Fatalf("before deadline refused: %v %v", created, err)
	}
}

func TestOwnerJournalConflictingAssumptionNeverReplacesWinner(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	conflicting := fixture.assumption
	accepted, err := canonicalStartupTime(fixture.intent.AcceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	conflicting.AssumedAt = accepted.Add(-time.Nanosecond).Format(time.RFC3339Nano)
	if created, putErr := fixture.journal.putAssumption(t.Context(), fixture.lease, conflicting); created || !errors.Is(putErr, task.ErrEvidenceFault) {
		t.Fatalf("conflicting child replaced assumption: %v %v", created, putErr)
	}
	saved, err := fixture.journal.readAssumption(t.Context(), fixture.assumption.Startup)
	if err != nil || saved != fixture.assumption {
		t.Fatalf("original assumption changed: %v", err)
	}
}

func TestOwnerJournalRejectsTamperedPublication(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication)
	if err != nil || !created {
		t.Fatalf("publication: %v %v", created, err)
	}
	changed := fixture.publication
	changed.RuntimeSHA256 = task.ComputeSHA256([]byte("different runtime"))
	data, err := task.MarshalCanonical(changed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.journal.pin.Root(), "infrastructure", publicationName(1))
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.journal.readPublication(t.Context(), fixture.publication.Startup); !errors.Is(err, ErrIdentity) {
		t.Fatalf("tampered publication accepted: %v", err)
	}
}

func TestOwnerJournalMissingAssumptionCannotPublish(t *testing.T) {
	fixture := ownerJournal(t)
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication); created || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication without assumption: %v %v", created, err)
	}
	path := filepath.Join(fixture.journal.pin.Root(), "infrastructure", publicationName(1))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-assumption refusal wrote publication: %v", err)
	}
}

func TestOwnerJournalAssumptionRequiresActualSelf(t *testing.T) {
	for _, changed := range []string{"pid", "uid", "token"} {
		t.Run(changed, func(t *testing.T) {
			checkOwnerJournalNonSelf(t, changed)
		})
	}
}

func checkOwnerJournalNonSelf(t *testing.T, changed string) {
	t.Helper()
	fixture := ownerJournal(t)
	assumption := fixture.assumption
	switch changed {
	case "pid":
		assumption.Process.PID++
	case "uid":
		assumption.UID++
	case "token":
		assumption.Process.StartToken += ":different"
	}
	if err := assumption.Validate(fixture.intent); err != nil {
		t.Fatalf("negative fixture is not valid record data: %v", err)
	}
	if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, assumption); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("non-self %s assumption accepted: %v %v", changed, created, err)
	}
	if _, err := fixture.journal.readAssumption(t.Context(), assumption.Startup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-self refusal wrote assumption: %v", err)
	}
}
