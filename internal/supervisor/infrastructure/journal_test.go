package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func journalStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(testOrigin(t).Root(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func journalFixture(t *testing.T) (*intentJournal, *Lease) {
	t.Helper()
	journal, err := openIntentJournal(t.Context(), journalStore(t), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("instance election: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return journal, lease
}

func journalIntent(t *testing.T, journal *intentJournal, sequence uint64) StartupIntent {
	t.Helper()
	intent := startupIntentFixture(t)
	intent.Sequence = sequence
	record := contextForJournal(t, journal)
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context = reference
	intent.Inputs.BootID = journal.bootID
	intent.Inputs.Root, intent.Inputs.RootID = journal.pin.Root(), journal.pin.RootID()
	intent.Inputs.RootDevice, intent.Inputs.RootInode = journal.pin.Device(), journal.pin.Inode()
	refreshIntentDigest(t, &intent)
	return intent
}

func refreshIntentDigest(t *testing.T, intent *StartupIntent) {
	t.Helper()
	digest, err := intent.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	intent.InputsSHA256 = digest
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
}

func requireJournalPut(t *testing.T, journal *intentJournal, lease *Lease, intent StartupIntent) {
	t.Helper()
	created, err := journal.putIntent(t.Context(), lease, intent)
	if err != nil || !created {
		t.Fatalf("intent publication: created=%v err=%v", created, err)
	}
}

func TestIntentJournalCanonicalPublication(t *testing.T) {
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	requireJournalPut(t, journal, lease, intent)
	created, err := journal.putIntent(t.Context(), lease, intent)
	if err != nil || created {
		t.Fatalf("equivalent publication: created=%v err=%v", created, err)
	}
	conflict := intent
	conflict.Deadline = "2026-11-03T00:00:31Z"
	if created, err = journal.putIntent(t.Context(), lease, conflict); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting publication: created=%v err=%v", created, err)
	}
	saved, err := journal.readIntent(t.Context(), 1)
	if err != nil || saved != intent {
		t.Fatalf("canonical winner changed: %v", err)
	}
	path := filepath.Join(journal.pin.Root(), "infrastructure", intentName(1))
	if err = os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err = journal.putIntent(t.Context(), lease, intent); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unreadable existing winner replaced: created=%v err=%v", created, err)
	}
	if _, err = journal.readIntent(t.Context(), 1); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable winner classified absent: %v", err)
	}
}

func TestIntentJournalRejectsWrongIdentity(t *testing.T) {
	journal, lease := journalFixture(t)
	for _, field := range []string{"root", "root-id", "device", "inode"} {
		t.Run(field, func(t *testing.T) {
			checkIntentWrongIdentity(t, journal, lease, field)
		})
	}
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused intent was written: %v", err)
	}
	other, otherLease := journalFixture(t)
	if created, err := journal.putIntent(t.Context(), otherLease, journalIntent(t, journal, 1)); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign lease accepted: %v", err)
	}
	if _, acquired, err := journal.tryAcquire(t.Context()); err != nil || acquired {
		t.Fatalf("occupied lease: acquired=%v err=%v", acquired, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if created, err := journal.putIntent(t.Context(), lease, journalIntent(t, journal, 1)); created || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed lease accepted: %v", err)
	}
	if other.pin.Equal(journal.pin) {
		t.Fatal("fixture roots unexpectedly equal")
	}
}

func TestIntentJournalLatestDerivesMaximumSequence(t *testing.T) {
	journal, _ := journalFixture(t)
	for _, sequence := range []uint64{2, 1, ^uint64(0)} {
		putHistoricalFixture(t, journal, journalIntent(t, journal, sequence))
	}
	for range 2 {
		latest, found, err := journal.latestIntent(t.Context())
		if err != nil || !found || latest.Sequence != ^uint64(0) {
			t.Fatalf("latest: found=%v sequence=%d err=%v", found, latest.Sequence, err)
		}
	}
	for _, name := range []string{"intent-1.json", "intent-00000000000000000000.json", "intent-18446744073709551616.json", "intent-00000000000000000003.json.bak"} {
		path := filepath.Join(journal.pin.Root(), "infrastructure", name)
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := journal.latestIntent(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("malformed intent name %q accepted: %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntentJournalMissingRequestVersusListedRecord(t *testing.T) {
	journal, lease := journalFixture(t)
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrNotExist) || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("requested missing record: %v", err)
	}
	requireJournalPut(t, journal, lease, journalIntent(t, journal, 1))
	reader := vanishingIntentReader{controlIntentReader{journal.control}, filepath.Join(journal.pin.Root(), "infrastructure", intentName(1))}
	if _, found, err := latestPinnedIntent(t.Context(), reader, journal.pin); found || !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listed record disappearance: found=%v err=%v", found, err)
	}
}

type vanishingIntentReader struct {
	controlIntentReader
	path string
}

func (r vanishingIntentReader) read(ctx context.Context, name string) (StartupIntent, error) {
	if err := os.Remove(r.path); err != nil {
		return StartupIntent{}, err
	}
	return r.controlIntentReader.read(ctx, name)
}

func TestIntentJournalPassiveAbsenceAndRead(t *testing.T) {
	store := journalStore(t)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var found bool
	if _, found, err = readLatestIntent(t.Context(), reader); err != nil || found {
		t.Fatalf("passive absence: found=%v err=%v", found, err)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, "infrastructure")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive read initialized journal: %v", err)
	}
	journal, err := openIntentJournal(t.Context(), store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("acquire: %v %v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	intent := journalIntent(t, journal, 1)
	requireJournalPut(t, journal, lease, intent)
	latest, found, err := readLatestIntent(t.Context(), reader)
	if err != nil || !found || latest != intent {
		t.Fatalf("passive canonical intent: found=%v err=%v", found, err)
	}
}

func TestIntentJournalRejectsReplacedRoot(t *testing.T) {
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	metadata, err := os.ReadFile(filepath.Join(journal.pin.Root(), "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	retained := journal.pin.Root() + "-retained"
	// The renamed original remains under the same test-owned temporary parent.
	if err = os.Rename(journal.pin.Root(), retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := journal.store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if removeErr := os.RemoveAll(retained); removeErr != nil {
			t.Error(removeErr)
		}
	})
	if err = os.Mkdir(journal.pin.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(journal.pin.Root(), "root.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = journal.tryAcquire(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replacement election: %v", err)
	}
	if created, putErr := journal.putIntent(t.Context(), lease, intent); created || putErr == nil {
		t.Fatalf("replacement write: created=%v err=%v", created, putErr)
	}
	if _, err = os.Lstat(filepath.Join(journal.pin.Root(), instanceLeaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("election mutated clone: %v", err)
	}
}

func TestIntentJournalRejectsCancelledAndClosed(t *testing.T) {
	journal, lease := journalFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := journal.tryAcquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled election: %v", err)
	}
	if _, err := journal.putIntent(ctx, lease, journalIntent(t, journal, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.tryAcquire(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed election: %v", err)
	}
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
	if _, _, err := (*intentJournal)(nil).latestIntent(t.Context()); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil journal: %v", err)
	}
}

func checkIntentWrongIdentity(t *testing.T, journal *intentJournal, lease *Lease, field string) {
	t.Helper()
	intent := journalIntent(t, journal, 1)
	switch field {
	case "root":
		intent.Inputs.Root = testOrigin(t).Root()
	case "root-id":
		intent.Inputs.RootID = originTestRootID
	case "device":
		intent.Inputs.RootDevice++
	case "inode":
		intent.Inputs.RootInode++
	}
	refreshIntentDigest(t, &intent)
	if created, err := journal.putIntent(t.Context(), lease, intent); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong %s accepted: created=%v err=%v", field, created, err)
	}
}

type replaceRootAtJournalOpen struct {
	taskdir.BaseFaultInjector
	root string
}

func (f *replaceRootAtJournalOpen) OnDirBarrier(path string) error {
	if path != filepath.Join(f.root, "infrastructure") {
		return nil
	}
	metadata, err := os.ReadFile(filepath.Join(f.root, "root.json"))
	if err != nil {
		return err
	}
	if err = os.Rename(f.root, f.root+"-retained"); err != nil {
		return err
	}
	if err = os.Mkdir(f.root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.root, "root.json"), metadata, 0o600)
}

func TestIntentJournalOpeningRechecksRoot(t *testing.T) {
	store := journalStore(t)
	store.SetFaultInjector(&replaceRootAtJournalOpen{root: store.Root})
	journal, err := openIntentJournal(t.Context(), store, true)
	if journal != nil {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Error("opening returned a journal after root replacement")
	}
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("post-open replacement: %v", err)
	}
}

func TestIntentJournalReadRejectsMismatchedSequence(t *testing.T) {
	journal, _ := journalFixture(t)
	intent := journalIntent(t, journal, 2)
	putHistoricalFixture(t, journal, intent)
	directory := filepath.Join(journal.pin.Root(), "infrastructure")
	if err := os.Rename(filepath.Join(directory, intentName(2)), filepath.Join(directory, intentName(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("filename sequence mismatch: %v", err)
	}
}

type blockIntentStage struct {
	taskdir.BaseFaultInjector
	entered chan struct{}
	release chan struct{}
}

func (f *blockIntentStage) OnStageBarrier(string) error {
	close(f.entered)
	<-f.release
	return nil
}

func TestIntentJournalCloseJoinsGuardedPublication(t *testing.T) {
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	fault := &blockIntentStage{entered: make(chan struct{}), release: make(chan struct{})}
	journal.store.SetFaultInjector(fault)
	written := make(chan error, 1)
	go func() { _, err := journal.putIntent(t.Context(), lease, intent); written <- err }()
	select {
	case <-fault.entered:
	case err := <-written:
		t.Fatalf("write ended before actual staging gate: %v", err)
	}
	// These are the actual ownership guards while canonical staging is active.
	if journal.mu.TryLock() {
		journal.mu.Unlock()
		t.Error("publication did not hold journal closure guard")
	}
	if lease.mu.TryLock() {
		lease.mu.Unlock()
		t.Error("publication did not hold lease closure guard")
	}
	closed := make(chan error, 1)
	go func() { closed <- journal.Close() }()
	close(fault.release)
	if err := <-written; err != nil {
		t.Error(err)
	}
	if err := <-closed; err != nil {
		t.Error(err)
	}
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("journal accepted read after joined Close: %v", err)
	}
}

func otherStableBoot(t *testing.T, current string) string {
	t.Helper()
	for _, candidate := range []string{
		"linux:11111111-2222-3333-4444-555555555555",
		"linux:22222222-3333-4444-5555-666666666666",
		"darwin:boot-session:11111111-2222-3333-4444-555555555555",
		"darwin:boot-session:22222222-3333-4444-5555-666666666666",
	} {
		if lifetime.CompareBootIDs(candidate, current) == lifetime.BootDifferent {
			return candidate
		}
	}
	t.Fatal("current native boot has no comparable stable fixture")
	return ""
}

func putHistoricalFixture(t *testing.T, journal *intentJournal, intent StartupIntent) string {
	t.Helper()
	err := journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		created, err := tx.Put(intentName(intent.Sequence), intent)
		if err == nil && !created {
			return errors.New("historical fixture was not created")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(journal.pin.Root(), "infrastructure", intentName(intent.Sequence))
}

func journalEntryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "infrastructure"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func TestIntentJournalReadsHistoricalBootWithoutRebinding(t *testing.T) {
	journal, lease := journalFixture(t)
	historical := journalIntent(t, journal, 1)
	historical.Inputs.BootID = otherStableBoot(t, journal.bootID)
	historical.Inputs.RootDevice++
	historical.Inputs.RootInode++
	refreshIntentDigest(t, &historical)
	path := putHistoricalFixture(t, journal, historical)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	names := journalEntryNames(t, journal.pin.Root())
	requireHistoricalIntentReads(t, journal, historical)
	if created, putErr := journal.putIntent(t.Context(), lease, historical); created || !errors.Is(putErr, ErrIdentity) {
		t.Fatalf("historical intent reused as current: %v %v", created, putErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("history bytes changed: %v", err)
	}
	if afterNames := journalEntryNames(t, journal.pin.Root()); !slices.Equal(names, afterNames) {
		t.Fatal("historical reads/write refusal changed journal entries")
	}
}

func TestIntentJournalRefusesOldBootBeforeNewPublication(t *testing.T) {
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	intent.Inputs.BootID = otherStableBoot(t, journal.bootID)
	refreshIntentDigest(t, &intent)
	if created, err := journal.putIntent(t.Context(), lease, intent); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("old boot publication: %v %v", created, err)
	}
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old boot refusal wrote a record: %v", err)
	}
}

func TestIntentJournalRejectsUnknownBootHistory(t *testing.T) {
	journal, _ := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	intent.Inputs.BootID = "unknown"
	putHistoricalFixture(t, journal, intent)
	if _, err := journal.readIntent(t.Context(), 1); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unknown boot history accepted: %v", err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, _, err = readLatestIntent(t.Context(), reader); !errors.Is(err, ErrIdentity) {
		t.Fatalf("passive unknown boot accepted: %v", err)
	}
}

func requireHistoricalIntentReads(t *testing.T, journal *intentJournal, historical StartupIntent) {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	saved, err := journal.readIntent(t.Context(), 1)
	if err != nil || saved != historical {
		t.Fatalf("historical direct read changed/refused: %v", err)
	}
	latest, found, err := journal.latestIntent(t.Context())
	if err != nil || !found || latest != historical {
		t.Fatalf("historical latest read: %v %v", found, err)
	}
	latest, found, err = readLatestIntent(t.Context(), reader)
	if err != nil || !found || latest != historical {
		t.Fatalf("passive historical read: %v %v", found, err)
	}
}

func appendIntentFixture(t *testing.T, journal *intentJournal, sequence uint64) StartupIntent {
	t.Helper()
	intent := journalIntent(t, journal, sequence)
	intent.Epoch = fmt.Sprintf("%032x", sequence)
	return intent
}

func TestIntentJournalAppendsAndReplaysWithoutReplacing(t *testing.T) {
	journal, lease := journalFixture(t)
	if created, err := journal.putIntent(t.Context(), lease, appendIntentFixture(t, journal, 2)); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("initial gap accepted: %v %v", created, err)
	}
	first := appendIntentFixture(t, journal, 1)
	requireJournalPut(t, journal, lease, first)
	if created, err := journal.putIntent(t.Context(), lease, appendIntentFixture(t, journal, 3)); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("later gap accepted: %v %v", created, err)
	}
	requireJournalPut(t, journal, lease, appendIntentFixture(t, journal, 2))
	if created, err := journal.putIntent(t.Context(), lease, first); created || err != nil {
		t.Fatalf("exact historical replay became fresh: %v %v", created, err)
	}
	conflicting := first
	conflicting.Deadline = "2026-11-03T01:00:00Z"
	if created, err := journal.putIntent(t.Context(), lease, conflicting); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting replay replaced history: %v %v", created, err)
	}
	latest, found, err := journal.latestIntent(t.Context())
	if err != nil || !found || latest.Sequence != 2 {
		t.Fatalf("refused writes changed latest: %v %v", found, err)
	}
}

func TestIntentJournalRefusesDescendingAndReusedEpoch(t *testing.T) {
	t.Run("descending", func(t *testing.T) {
		journal, lease := journalFixture(t)
		putHistoricalFixture(t, journal, appendIntentFixture(t, journal, 2))
		if created, err := journal.putIntent(t.Context(), lease, appendIntentFixture(t, journal, 1)); created || !errors.Is(err, ErrIdentity) {
			t.Fatalf("descending invisible intent accepted: %v %v", created, err)
		}
	})
	t.Run("reused-epoch", func(t *testing.T) {
		journal, lease := journalFixture(t)
		first := appendIntentFixture(t, journal, 1)
		requireJournalPut(t, journal, lease, first)
		next := appendIntentFixture(t, journal, 2)
		next.Epoch = first.Epoch
		if created, err := journal.putIntent(t.Context(), lease, next); created || !errors.Is(err, ErrIdentity) {
			t.Fatalf("epoch reuse accepted: %v %v", created, err)
		}
	})
}

func TestIntentJournalSequenceExhaustionRetainsHistory(t *testing.T) {
	journal, lease := journalFixture(t)
	last := appendIntentFixture(t, journal, ^uint64(0))
	putHistoricalFixture(t, journal, last)
	if created, err := journal.putIntent(t.Context(), lease, last); created || err != nil {
		t.Fatalf("max-sequence exact replay: %v %v", created, err)
	}
	if created, err := journal.putIntent(t.Context(), lease, appendIntentFixture(t, journal, 1)); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("exhausted sequence wrapped: %v %v", created, err)
	}
	latest, found, err := journal.latestIntent(t.Context())
	if err != nil || !found || latest != last {
		t.Fatalf("overflow changed latest history: %v %v", found, err)
	}
}

func (j *intentJournal) tryAcquire(ctx context.Context) (lease *Lease, acquired bool, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		lease, acquired, err = TryAcquireLease(ctx, filepath.Join(j.pin.Root(), instanceLeaseName))
		if err != nil {
			return err
		}
		if err = j.checkRoot(ctx); err != nil {
			if lease != nil {
				err = errors.Join(err, lease.Close())
			}
			lease, acquired = nil, false
			return err
		}
		return nil
	})
	return lease, acquired, resultErr
}
