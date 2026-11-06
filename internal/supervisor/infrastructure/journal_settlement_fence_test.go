package infrastructure

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func settledUnassumedOwnerFixture(t *testing.T) ownerJournalFixture {
	t.Helper()
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	now := time.Now().UTC()
	intent.AcceptedAt = now.Add(-2 * time.Second).Format(time.RFC3339Nano)
	intent.Deadline = now.Add(-time.Second).Format(time.RFC3339Nano)
	refreshIntentDigest(t, &intent)
	requireJournalPut(t, journal, lease, intent)
	assumption, publication := evidenceForIntent(t, intent)
	record, created, err := journal.settleStartup(t.Context(), lease, assumption.Startup)
	if err != nil || !created || record.Reason != settlementUnassumedExpired {
		t.Fatalf("unassumed expiry fixture did not settle: %v %v", created, err)
	}
	return ownerJournalFixture{journal, lease, intent, assumption, publication}
}

func TestOwnerJournalSettlementFencesBackwardsClockAssumption(t *testing.T) {
	fixture := settledUnassumedOwnerFixture(t)
	deadline, err := canonicalStartupTime(fixture.intent.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	// This is a journal regression, with no child launch. The injected clock
	// models a step backwards after durable settlement, not a renewed deadline.
	created, err := fixture.journal.putAssumptionWithClock(t.Context(), fixture.lease, fixture.assumption, func() time.Time {
		return deadline.Add(-time.Nanosecond)
	})
	if created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("settled attempt assumed after clock rollback: %v %v", created, err)
	}
	if _, err = fixture.journal.readAssumption(t.Context(), fixture.assumption.Startup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected late assumption changed its journal: %v", err)
	}
}

func TestOwnerJournalSettlementFencesLatePublication(t *testing.T) {
	fixture := settledUnassumedOwnerFixture(t)
	// Emulate the old writer's late assumption. The publication edge must fence
	// independently; this stored fixture grants no native permission.
	if err := fixture.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(assumptionName(fixture.intent.Sequence), fixture.assumption)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.journal.pin.Root(), "infrastructure", settlementName(fixture.intent.Sequence))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication)
	if created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("settled attempt published after settlement: %v %v", created, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected publication altered settlement: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(fixture.journal.pin.Root(), "infrastructure", publicationName(fixture.intent.Sequence))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled attempt created a publication: %v", err)
	}
}
