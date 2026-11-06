package infrastructure

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestWarmOwnerBoundContactRejectsReplacementBeforeOwnerRecords(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpublished-S2", true: "published-S2"}[published], func(t *testing.T) {
			f := producerFixture(t)
			old := seedProducerIntent(t, f, 1, false)
			seedWarmOwnerEvidence(t, f, old, true)
			expected := settlementReference(t, old)
			if _, _, err := f.journal.settleStartup(t.Context(), f.lease, expected); err != nil {
				t.Fatal(err)
			}
			current := seedProducerIntent(t, f, 2, false)
			if published {
				seedWarmOwnerEvidence(t, f, current, false)
			}
			reader := contextPassiveReader(t, f.journal)
			before := producerBudgetTree(t, f.journal.pin.Root())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			outcome, err := RequestBoundPreparation(ctx, reader, expected, PreparationRequest{Context: current.Inputs.Context})
			if !errors.Is(err, task.ErrLockBusy) || task.RecordAbsent(err) || outcome != (PreparationOutcome{}) {
				t.Fatalf("bound contact selected replacement owner records: outcome=%v err=%v", outcome, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("bound refusal mutated replaced owner state")
			}
		})
	}
}

func TestWarmOwnerBoundContactRejectsStaleReferenceBeforeDial(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stale := f.auth.request.Owner
	stale.Sequence++
	for _, expected := range []StartupReference{stale, {}} {
		outcome, err := RequestBoundPreparation(ctx, f.reader, expected, PreparationRequest{Context: f.auth.intent.Inputs.Context})
		want := task.ErrLockBusy
		if expected == (StartupReference{}) {
			want = ErrIdentity
		}
		if !errors.Is(err, want) || outcome != (PreparationOutcome{}) {
			t.Fatalf("stale/zero bound contact was sent: outcome=%v err=%v", outcome, err)
		}
		requireNoOwnerContactConnection(t, f.auth.listener)
	}
}

func TestWarmOwnerRetainedAttemptTypedCauseNeverContacts(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// This isolated cleanup fault has no native child. Production-store reader
	// closure is completed by finishProducerFailure; its sticky error retains the
	// attempt result and must prevent the consumer from following the typed cause.
	partial := &StartupAttempt{reader: contextPassiveReader(t, f.journal), closeErr: task.ErrLockBusy}
	retained, err := finishProducerFailure(partial, &OwnerContactRequiredError{Startup: f.auth.request.Owner})
	if retained != partial || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("fixture lost retained producer result: %v", err)
	}
	established, resultErr := prepareEstablishedRoot(ctx, retained, err)
	if !errors.Is(resultErr, err) || established != (EstablishedOwner{}) {
		t.Fatalf("retained partial attempt followed contact cause: owner=%v err=%v", established, resultErr)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}
