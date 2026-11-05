package infrastructure

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestProducerWaitForContactPendingPublicationAndNativeExit(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-exit", true: "publication"}[publish], func(t *testing.T) { exerciseProducerWaitForContact(t, publish) })
	}
}

func exerciseProducerWaitForContact(t *testing.T, publish bool) {
	t.Helper()
	f := newProducerNativeFixture(t)
	attempt, err := establishOutside(t.Context(), f.store, f.request, f.host, f.nativeForChild(t, nil, "TestProducerNativePendingChild"))
	if attempt != nil {
		f.attempts = append(f.attempts, attempt)
	}
	if err != nil || attempt == nil {
		t.Fatalf("accept pending native owner: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticks := make(chan time.Time)
	done := startProducerContactWait(t, f, attempt, ctx, cancel, ticks)
	// A tick cannot be received until the canonical first Observe found pending.
	select {
	case ticks <- time.Now():
	case err = <-done:
		done <- err
		t.Fatalf("wait did not observe pending: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseProducerPendingContact(t, f, attempt, ctx, ticks, publish)
	select {
	case err = <-done:
		done <- err
	case <-ctx.Done():
		t.Fatal("wait ignored native publication/completion")
	}
	if publish && err != nil || !publish && !errors.Is(err, ErrIdentity) {
		t.Fatalf("publication=%t wait=%v", publish, err)
	}
	if f.host.calls != 1 || attempt.intent.Deadline != f.request.Deadline.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("wait restarted or renewed owner: starts=%d deadline=%s", f.host.calls, attempt.intent.Deadline)
	}
}

func TestProducerWaitForContactJoinedCancellationAndRecordFailure(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	if err != nil || attempt == nil || attempt.NativeCompletion() != nil {
		t.Fatalf("join: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	cancel()
	if err = attempt.WaitForContact(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err = attempt.Close(); err != nil {
		t.Fatal(err)
	}
	finite, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err = attempt.WaitForContact(finite); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed attempt became contactable")
	}
	if f.host.calls != 0 || attempt.Reference() != settlementReference(t, intent) {
		t.Fatal("wait changed joined owner")
	}
}

func startProducerContactWait(t *testing.T, f *producerNativeFixture, attempt *StartupAttempt, ctx context.Context, cancel context.CancelFunc, ticks <-chan time.Time) chan error {
	t.Helper()

	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			f.retain = true
			t.Error("contact waiter did not join")
		}
	})
	go func() { done <- attempt.waitForContact(ctx, ticks) }()
	return done
}

func releaseProducerPendingContact(t *testing.T, f *producerNativeFixture, attempt *StartupAttempt, ctx context.Context, ticks chan<- time.Time, publish bool) {
	t.Helper()

	if publish {
		if _, err := f.stop.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		f.ready(t, attempt)
		select {
		case ticks <- time.Now():
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		if err := f.stop.Close(); err != nil {
			t.Fatal(err)
		}
		f.stop = nil
	}
}
