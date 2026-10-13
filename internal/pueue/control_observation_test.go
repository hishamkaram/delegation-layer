package pueue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSettlePendingControlObservationReconcilesAfterPendingStatus(t *testing.T) {
	client := &Client{options: Options{ObservationTimeout: time.Second}}
	pending := &Pending{done: make(chan struct{})}
	firstObservation := make(chan struct{})
	complete := sync.OnceFunc(func() {
		pending.result = CommandResult{Started: true, ExitCode: 0}
		close(pending.done)
	})
	t.Cleanup(complete)
	go func() {
		<-firstObservation
		complete()
	}()

	calls := 0
	observation, err := settlePendingControlObservation(client, t.Context(), func(context.Context) (int, error) {
		calls++
		switch calls {
		case 1:
			close(firstObservation)
			return 1, errors.Join(ErrUnknown, &InFlightError{Pending: pending})
		case 2:
			select {
			case <-pending.Done():
			default:
				t.Fatal("reconciliation retried before the original status command completed")
			}
			return 2, nil
		default:
			t.Fatalf("reconciliation called %d times", calls)
			return 0, nil
		}
	})
	if err != nil {
		t.Fatalf("settled status observation returned error: %v", err)
	}
	if observation != 2 || calls != 2 {
		t.Fatalf("settled status observation = %d after %d calls, want 2 after 2 calls", observation, calls)
	}
}

func TestSettlePendingControlObservationRetainsUnsettledStatus(t *testing.T) {
	client := &Client{options: Options{ObservationTimeout: time.Hour}}
	pending := &Pending{done: make(chan struct{})}
	t.Cleanup(func() { close(pending.done) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	observation, err := settlePendingControlObservation(client, ctx, func(context.Context) (int, error) {
		calls++
		cancel()
		return 1, errors.Join(ErrUnknown, &InFlightError{Pending: pending})
	})
	if observation != 1 || calls != 1 {
		t.Fatalf("unsettled status observation = %d after %d calls, want 1 after 1 call", observation, calls)
	}
	if !errors.Is(err, ErrUnknown) || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrInFlight) {
		t.Fatalf("unsettled status lost its safe uncertainty: %v", err)
	}
	if pendingFrom(err) != pending {
		t.Fatalf("unsettled status lost its pending handle: %v", err)
	}
}
