package infrastructure

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var _ pueue.DaemonWaitSettlementOwner = (*daemonWaitOwner)(nil)

func TestObserveDaemonWaitUsesExactBirthAndActualCompletion(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, func() (lifetime.ProcessRef, bool, error) { return expected, true, nil }); err != nil {
		t.Fatal(err)
	}
	owned, closed, err := w.ObserveDaemonWait(t.Context(), f.root, expected)
	if err != nil || !owned || closed {
		t.Fatalf("exact live Wait was not authoritative: owned=%t closed=%t err=%v", owned, closed, err)
	}
	close(done)
	owned, closed, err = w.ObserveDaemonWait(t.Context(), f.root, expected)
	if err != nil || !owned || !closed {
		t.Fatalf("real Wait completion was lost: owned=%t closed=%t err=%v", owned, closed, err)
	}
	// Normal activity prunes done handles. The missing map entry must never be
	// presented as Wait completion, even with exhaustive no-custodian knowledge.
	w.hasActiveWait()
	other := make(chan struct{})
	defer close(other)
	if err = w.RegisterDaemonWait(f.root, expected.PID()+1, other); err != nil {
		t.Fatal(err)
	}
	owned, closed, err = w.ObserveDaemonWait(t.Context(), f.root, expected)
	if err != nil || owned || closed {
		t.Fatalf("pruned predecessor became a fabricated Wait or successor blocker: owned=%t closed=%t err=%v", owned, closed, err)
	}
}

func TestObserveDaemonWaitUnfinishedBirthAndFaultNeverMeanUnowned(t *testing.T) {
	for _, kind := range []string{"pending", "fault", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			f, done, expected := daemonBirthFixture(t)
			fault := errors.New("immutable birth unavailable")
			var err error
			if kind == "legacy" {
				err = f.server.waits.RegisterDaemonWait(f.root, expected.PID(), done)
			} else {
				err = f.server.waits.RegisterDaemonWaitBirth(f.root, expected.PID(), done, func() (lifetime.ProcessRef, bool, error) {
					if kind == "fault" {
						return lifetime.ProcessRef{}, true, fault
					}
					return lifetime.ProcessRef{}, false, nil
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
			if err == nil || owned || closed || kind == "fault" && !errors.Is(err, fault) || historicalWaitOriginCount(err) != 0 {
				t.Fatalf("uncertain birth became known-unowned: owned=%t closed=%t err=%v", owned, closed, err)
			}
		})
	}
}

// This real-store history fixture has three attempts. Attempt one is an actual
// same-process published/released owner. Attempt two records a known absent
// incarnation (or a fenced unassumed attempt); no child or daemon is launched.
// The current third attempt is published with actual self/native socket facts.
func waitHistoryFixture(t *testing.T, unassumed bool) *ownerServerFixture {
	t.Helper()
	return waitHistoryFixtureWithSecond(t, unassumed, false)
}

func waitHistoryFixtureWithSecond(t *testing.T, unassumed, liveSecond bool) *ownerServerFixture {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	original := f.entry.operation.Intent()
	selected := f.birthContext(t)
	if err := f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	if released, err := f.entry.releaseServingContext(t.Context()); err != nil || !released {
		t.Fatalf("fixture serving release: %t %v", released, err)
	}
	store, err := taskdir.OpenExistingStoreMetadataContext(t.Context(), f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	f.entry = &ownedResidentEntry{store: store}
	f.entry.journal, err = openIntentJournal(t.Context(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	f.entry.lease, acquired, err = tryAcquireExistingLease(t.Context(), filepath.Join(f.root, instanceLeaseName))
	if err != nil || !acquired {
		t.Fatalf("fixture successor lease: %t %v", acquired, err)
	}
	j := f.entry.journal
	second := original
	second.Sequence, second.Epoch = 2, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if unassumed {
		second.AcceptedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
		second.Deadline = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	}
	requireJournalPut(t, j, f.entry.lease, second)
	writeWaitHistorySecond(t, f, second, unassumed, liveSecond)
	third := original
	third.Sequence, third.Epoch = 3, "cccccccccccccccccccccccccccccccc"
	requireJournalPut(t, j, f.entry.lease, third)
	f.entry.operation, err = newRootOwnerOperation(third, selected)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := ReferenceStartup(third)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.entry.assumeAndPublish(t.Context(), reference); err != nil {
		t.Fatal(err)
	}
	f.server, err = newOwnerServer(t.Context(), f.entry, &ownerServerTestHost{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func writeWaitHistorySecond(t *testing.T, f *ownerServerFixture, intent StartupIntent, unassumed, liveSecond bool) {
	t.Helper()
	j := f.entry.journal
	reference, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	evidence := settlementEvidence{intent: intent}
	reason := settlementUnassumedExpired
	if !unassumed {
		assumption, publication := evidenceForIntent(t, intent)
		if !liveSecond {
			assumption.Process.StartToken = "known-absent-historical-incarnation"
		}
		publication.AssumptionSHA256, err = assumption.Digest(intent)
		if err != nil {
			t.Fatal(err)
		}
		evidence.assumption, evidence.publication = &assumption, &publication
		reason = settlementServingReleased
	}
	record := startupSettlement{Protocol: RecordProtocolVersion, Startup: reference, ObservedBootID: j.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: reason}
	if err = evidence.bind(&record); err != nil {
		t.Fatal(err)
	}
	if err = evidence.validate(record); err != nil {
		t.Fatal(err)
	}
	if err = j.withLeaseTransaction(t.Context(), f.entry.lease, func(tx *taskdir.ControlTransaction) error {
		if evidence.assumption != nil {
			if _, putErr := tx.Put(assumptionName(2), *evidence.assumption); putErr != nil {
				return putErr
			}
			if _, putErr := tx.Put(publicationName(2), *evidence.publication); putErr != nil {
				return putErr
			}
		}
		_, putErr := tx.Put(settlementName(2), record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
}

func TestObserveDaemonWaitChecksAllRotationsBeyondImmediateRetiredOwner(t *testing.T) {
	for _, unassumed := range []bool{false, true} {
		f := waitHistoryFixture(t, unassumed)
		before := producerBudgetTree(t, f.root)
		expected, err := lifetime.NewProcessRef(101, "predecessor-birth")
		if err != nil {
			t.Fatal(err)
		}
		owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
		if !errors.Is(err, ErrOwnerPending) || owned || closed || historicalWaitOriginCount(err) != 1 {
			t.Fatalf("older live owner was lost behind absent/unassumed immediate owner: owned=%t closed=%t err=%v", owned, closed, err)
		}
		if !maps.Equal(before, producerBudgetTree(t, f.root)) {
			t.Fatal("Wait observation changed history or created files")
		}
	}
}

func TestObserveDaemonWaitHistoryFaultCannotGrantNativeFallback(t *testing.T) {
	for _, damaged := range []string{intentName(2), assumptionName(2), settlementName(2)} {
		t.Run(damaged, func(t *testing.T) {
			f := waitHistoryFixture(t, false)
			path := filepath.Join(f.root, "infrastructure", damaged)
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			expected, err := lifetime.NewProcessRef(101, "predecessor-birth")
			if err != nil {
				t.Fatal(err)
			}
			owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
			origins := 0
			if damaged == settlementName(2) {
				// This fault is reached only after the older live custodian.
				origins = 1
			}
			if !errors.Is(err, task.ErrEvidenceFault) || historicalWaitOriginCount(err) != origins || owned || closed {
				t.Fatalf("history fault lost evidence/origin or granted fallback: owned=%t closed=%t origins=%d want=%d err=%v", owned, closed, historicalWaitOriginCount(err), origins, err)
			}
		})
	}
}

func TestObserveDaemonWaitCanceledAndForeignRootCannotGrantFallback(t *testing.T) {
	f, _, expected := daemonBirthFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if owned, closed, err := f.server.waits.ObserveDaemonWait(ctx, f.root, expected); !errors.Is(err, context.Canceled) || owned || closed {
		t.Fatalf("canceled observation granted fallback: %t %t %v", owned, closed, err)
	}
	if owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), "/other-root", expected); !errors.Is(err, ErrIdentity) || owned || closed {
		t.Fatalf("foreign observation granted fallback: %t %t %v", owned, closed, err)
	}
	if err := validateWaitHistoryInventory([]string{intentName(1), intentName(3)}, 3); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("gapped inventory accepted: %v", err)
	}
}

func TestObserveDaemonWaitFencedUnassumedHistoryCanProveNoCustodian(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: InitialRuntimeRestart})
	j := f.entry.journal
	intent, err := j.readIntent(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := canonicalStartupTime(intent.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	record := startupSettlement{
		Protocol: RecordProtocolVersion, Startup: reference, ObservedBootID: j.bootID,
		SettledAt: deadline.Add(time.Second).UTC().Format(time.RFC3339Nano), Reason: settlementUnassumedExpired,
	}
	if err = record.Validate(intent); err != nil {
		t.Fatal(err)
	}
	if err = j.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(settlementName(1), record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	expected, err := lifetime.NewProcessRef(101, "predecessor-birth")
	if err != nil {
		t.Fatal(err)
	}
	before := producerBudgetTree(t, f.root)
	owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
	if err != nil || owned || closed {
		t.Fatalf("fenced pre-launch attempt invented Wait custody: %t %t %v", owned, closed, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("no-custodian proof mutated the pre-launch evidence")
	}
}

type waitHistoryDisappearance struct {
	ownerRecordReader
	remove func() error
}

func (r *waitHistoryDisappearance) names(ctx context.Context) ([]string, error) {
	names, err := r.ownerRecordReader.names(ctx)
	if err == nil && r.remove != nil {
		remove := r.remove
		r.remove = nil
		err = remove()
	}
	return names, err
}

func TestObserveDaemonWaitListedPublicationDisappearanceIsFault(t *testing.T) {
	f := waitHistoryFixture(t, false)
	j := f.entry.journal
	reader := &waitHistoryDisappearance{ownerRecordReader: controlOwnerReader{controlIntentReader{j.control}}, remove: func() error {
		return os.Remove(filepath.Join(f.root, "infrastructure", publicationName(2)))
	}}
	current, err := ReferenceStartup(f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	err = observeHistoricalCustodians(t.Context(), j, reader, j.pin, current)
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listed disappearance became partial-before-launch absence: %v", err)
	}
}

type faultingWaitHistoryReader struct {
	ownerRecordReader
	failure           error
	assumptionFailure error
}

func (r faultingWaitHistoryReader) read(ctx context.Context, name string) (StartupIntent, error) {
	if r.failure != nil {
		return StartupIntent{}, r.failure
	}
	return r.ownerRecordReader.read(ctx, name)
}

func (r faultingWaitHistoryReader) assumption(ctx context.Context, name string) (StartupAssumption, error) {
	if r.assumptionFailure != nil {
		return StartupAssumption{}, r.assumptionFailure
	}
	return r.ownerRecordReader.assumption(ctx, name)
}

func TestObserveDaemonWaitPendingCannotHideIndependentReaderFault(t *testing.T) {
	j, lease := journalFixture(t)
	intent := liveJournalIntent(t, j)
	requireJournalPut(t, j, lease, intent)
	current, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	// The real journal contains only an accepted, not-yet-assumed startup. Its
	// inventory is valid; the injected read failure reaches historical iteration.
	storageFault := errors.New("independent history read failure")
	reader := faultingWaitHistoryReader{ownerRecordReader: controlOwnerReader{controlIntentReader{j.control}}, failure: errors.Join(ErrOwnerPending, storageFault)}
	err = observeHistoricalCustodians(t.Context(), j, reader, j.pin, current)
	if !errors.Is(err, storageFault) || !errors.Is(err, ErrOwnerPending) || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("pending hid an independently joined storage fault: %v", err)
	}
}

func TestObserveDaemonWaitPendingCannotDiscardRetainedReaderOwner(t *testing.T) {
	j, lease := journalFixture(t)
	intent := liveJournalIntent(t, j)
	requireJournalPut(t, j, lease, intent)
	current, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	retained := &retainedPreparationTestError{store: j.store, control: j.control, cause: ErrOwnerPending}
	reader := faultingWaitHistoryReader{ownerRecordReader: controlOwnerReader{controlIntentReader{j.control}}, assumptionFailure: retained}
	err = observeHistoricalCustodians(t.Context(), j, reader, j.pin, current)
	var observed *retainedPreparationTestError
	if !errors.As(err, &observed) || observed != retained || !errors.Is(err, ErrOwnerPending) {
		t.Fatalf("pending discarded a retained history cleanup owner: %v", err)
	}
}

// A marker is an error-graph node, not an independent leaf or Wait handle.
func historicalWaitOriginCount(err error) int {
	if err == nil {
		return 0
	}
	if _, marked := err.(interface{ HistoricalDaemonWaitPending() }); marked {
		return 1
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		count := 0
		for _, child := range joined.Unwrap() {
			count += historicalWaitOriginCount(child)
		}
		return count
	}
	return historicalWaitOriginCount(errors.Unwrap(err))
}

func TestObserveDaemonWaitPreservesEveryHistoricalPendingOrigin(t *testing.T) {
	f := waitHistoryFixtureWithSecond(t, false, true)
	before := producerBudgetTree(t, f.root)
	expected, err := lifetime.NewProcessRef(101, "predecessor-birth")
	if err != nil {
		t.Fatal(err)
	}
	owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
	if !errors.Is(err, ErrOwnerPending) || owned || closed || historicalWaitOriginCount(err) != 2 {
		t.Fatalf("separate native-present historians lost their origin: owned=%t closed=%t origins=%d err=%v", owned, closed, historicalWaitOriginCount(err), err)
	}
	if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrOwnerPending) }) {
		t.Fatalf("origin marker changed pending leaf graph: %v", err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("historical pending observation changed records")
	}
}

func TestObserveDaemonWaitLaterFaultRetainsHistoricalPendingOrigin(t *testing.T) {
	f := waitHistoryFixture(t, false)
	// Absence is captured before inventory enumeration. The complete scan must
	// still reject a missing later settlement after observing the live old owner.
	if err := os.Remove(filepath.Join(f.root, "infrastructure", settlementName(2))); err != nil {
		t.Fatal(err)
	}
	before := producerBudgetTree(t, f.root)
	expected, err := lifetime.NewProcessRef(101, "predecessor-birth")
	if err != nil {
		t.Fatal(err)
	}
	owned, closed, err := f.server.waits.ObserveDaemonWait(t.Context(), f.root, expected)
	if !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, ErrOwnerPending) || historicalWaitOriginCount(err) != 1 || owned || closed {
		t.Fatalf("later fault discarded origin or granted native fallback: owned=%t closed=%t origins=%d err=%v", owned, closed, historicalWaitOriginCount(err), err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("faulting historical observation changed records")
	}
}
