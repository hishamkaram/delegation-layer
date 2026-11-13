package infrastructure

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestWarmOwnerJoinedPostObservationRefusesReplacedStartup(t *testing.T) {
	f := newProducerNativeFixture(t)
	attempt, err := producerInvocation(t).establishOutside(t.Context(), f.store, f.request, f.host, f.native(t, nil))
	if attempt != nil {
		f.attempts = append(f.attempts, attempt)
	}
	if err != nil || attempt == nil {
		t.Fatalf("native S1 establishment: %v", err)
	}
	f.ready(t, attempt)
	// This passive observer holds no native Wait or instance ownership.
	joined := &StartupAttempt{target: f.store, pin: attempt.pin, intent: attempt.intent}
	f.attempts = append(f.attempts, joined)
	joined.reader, err = taskdir.OpenReadOnlyStoreContext(t.Context(), f.store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if ready, observeErr := joined.Observe(ctx); observeErr != nil || !ready || joined.NativeCompletion() != nil {
		t.Fatalf("joined native S1 observation: ready=%t err=%v", ready, observeErr)
	}
	expected := joined.Reference()
	closeWarmOwnerNativeChild(t, f, attempt)
	successor := warmOwnerSuccessorJournal(t, f)
	if _, _, err = successor.journal.settleStartup(t.Context(), successor.lease, expected); err != nil {
		t.Fatal(err)
	}
	current := seedProducerIntent(t, successor, 2, false)
	// S2 has not assumed or published. A real listener at its future endpoint
	// must see no contact; the refusal cannot depend on missing owner records.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: ownerEndpointPath(f.store.Root, current.Sequence), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// This is a fresh caller after native closure and successor setup. Keep its
	// contact budget independent of the earlier passive observation's deadline.
	contact, cancelContact := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelContact()
	outcome, err := RequestBoundPreparation(contact, joined.reader, expected, PreparationRequest{Context: current.Inputs.Context})
	if !errors.Is(err, task.ErrLockBusy) || task.RecordAbsent(err) || outcome != (PreparationOutcome{}) {
		t.Fatalf("post-observation contact selected S2: outcome=%v err=%v", outcome, err)
	}
	requireNoOwnerContactConnection(t, listener)
}

func closeWarmOwnerNativeChild(t *testing.T, f *producerNativeFixture, attempt *StartupAttempt) {
	t.Helper()
	if err := f.stop.Close(); err != nil {
		t.Fatal(err)
	}
	f.stop = nil
	select {
	case <-attempt.NativeCompletion():
		if waitErr, complete := attempt.NativeWaitError(); !complete || waitErr != nil {
			t.Fatalf("native S1 Wait incomplete or failed: %v", waitErr)
		}
	case <-time.After(10 * time.Second):
		f.retain = true
		t.Fatal("native S1 did not close after its cooperative release")
	}
}

func warmOwnerSuccessorJournal(t *testing.T, f *producerNativeFixture) producerDataFixture {
	t.Helper()
	journal, err := openIntentJournal(t.Context(), f.store, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, acquired, err := journal.tryAcquire(t.Context())
	if lease != nil {
		t.Cleanup(func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil || !acquired {
		t.Fatalf("native S1 closed but successor lease unavailable: %t %v", acquired, err)
	}
	return producerDataFixture{journal: journal, lease: lease, request: f.request, host: f.host}
}
