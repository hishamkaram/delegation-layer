package infrastructure

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifecycleFreshZeroOwnerRefusedWithStartupHistory(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, fixture.publication); err != nil || !created {
		t.Fatalf("published current owner: created=%t err=%v", created, err)
	}
	body := journalLifecycleBody(t, fixture.journal, epochLifecycleStop, 1)
	requireZeroOwnerLifecycleRefusal(t, fixture.journal, fixture.lease, body)
	// The same body with the valid current reference must reach acceptance;
	// an unrelated namespace, deadline or native-pin fault cannot pass this test.
	body.Owner = fixture.assumption.Startup
	requireLifecycleAccept(t, fixture.journal, fixture.lease, body)
}

func TestLifecycleAbandonedNonzeroOwnerCannotBecomeZero(t *testing.T) {
	fixture, completed, original := lifecycleUpgradeFixture(t)
	// The shared fixture records a genuinely observed, exactly joined native
	// incarnation; it models historical records, not managed resident birth.
	completeDeadUpgradeHistory(t, fixture, completed, original)
	abandoned := journalLifecycleBody(t, fixture.journal, epochLifecycleOwnerUpgrade, 2)
	abandoned.Owner = settlementReference(t, original)
	requireLifecycleAccept(t, fixture.journal, fixture.lease, abandoned)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, abandoned, epochLifecycleClosing)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, abandoned, epochLifecycleExiting)
	later := lifecycleUpgradeStartup(t, fixture, abandoned, 2, true)
	if _, created, err := fixture.journal.settleStartup(t.Context(), fixture.lease, settlementReference(t, later)); err != nil || !created {
		t.Fatalf("settled later attempt: created=%t err=%v", created, err)
	}
	requireLifecycleProgress(t, fixture.journal, fixture.lease, abandoned, epochLifecycleAbandoned)
	snapshot, found, err := fixture.journal.latestLifecycle(t.Context())
	if err != nil || !found || snapshot.phase != epochLifecycleAbandoned || snapshot.intent.Owner == (StartupReference{}) {
		t.Fatalf("nonzero abandoned predecessor missing: %v", err)
	}
	if abandoned.Owner == settlementReference(t, later) {
		t.Fatal("positive control must distinguish original owner from later attempt")
	}
	next := journalLifecycleBody(t, fixture.journal, epochLifecycleStop, 3)
	requireZeroOwnerLifecycleRefusal(t, fixture.journal, fixture.lease, next)
	// Keep the predecessor's original owner even though the latest startup is
	// the later failed successor. Its actual incarnation is already absent.
	next.Owner = abandoned.Owner
	requireLifecycleAccept(t, fixture.journal, fixture.lease, next)
	if created, continueErr := fixture.journal.appendLifecycleProgress(t.Context(), fixture.lease, lifecycleReference(t, abandoned), lifecycleProgress(t, abandoned, epochLifecycleAbandoned)); created || !errors.Is(continueErr, ErrIdentity) {
		t.Fatalf("old continuation escaped new body: created=%t err=%v", created, continueErr)
	}
}

func requireZeroOwnerLifecycleRefusal(t *testing.T, journal *intentJournal, lease *Lease, body epochLifecycleIntent) {
	t.Helper()
	if body.Owner != (StartupReference{}) {
		t.Fatal("negative fixture must request a zero logical owner")
	}
	if created, err := journal.acceptLifecycle(t.Context(), lease, body); created || !errors.Is(err, ErrIdentity) {
		t.Fatalf("new zero owner inferred from existing history: created=%t err=%v", created, err)
	}
	var saved epochLifecycleIntent
	if err := journal.control.Read(lifecycleName(body.ID), &saved); !task.RecordAbsent(err) {
		t.Fatalf("zero-owner refusal wrote a body: %v", err)
	}
}
