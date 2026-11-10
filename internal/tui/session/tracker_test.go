package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStopCancelsReadsButNeverEffects(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	tracker := New(parent)
	readDone := make(chan error, 1)
	effectContext := make(chan context.Context, 1)
	release := make(chan struct{})
	effectDone := make(chan error, 1)
	t.Cleanup(func() {
		close(release)
		select {
		case <-effectDone:
		case <-time.After(time.Second):
			t.Error("effect did not settle")
		}
	})
	if _, err := tracker.Start(Read, Reference{}, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(_ uint64, err error) { readDone <- err }); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.Start(Effect, Reference{Description: "admission", Command: []string{"status", "id"}}, func(ctx context.Context) error { effectContext <- ctx; <-release; return nil }, func(_ uint64, err error) { effectDone <- err }); err != nil {
		t.Fatal(err)
	}
	ctx := <-effectContext
	cancel()
	tracker.Stop()
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("read: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("effect cancelled: %v", ctx.Err())
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("session invented effect deadline")
	}
	pending := tracker.Pending()
	if len(pending) != 1 || pending[0].Reference.Description != "admission" {
		t.Fatalf("pending=%+v", pending)
	}
	pending[0].Reference.Command[0] = "changed"
	if tracker.Pending()[0].Reference.Command[0] != "status" {
		t.Fatal("mutable pending snapshot")
	}
	if _, err := tracker.Start(Read, Reference{}, func(context.Context) error { return nil }, nil); !errors.Is(err, ErrClosing) {
		t.Fatalf("late start=%v", err)
	}
}

func TestWorkerPanicIsReportedAndRegistrationCloses(t *testing.T) {
	tracker := New(t.Context())
	t.Cleanup(tracker.Stop)
	done := make(chan error, 1)
	if _, err := tracker.Start(Read, Reference{}, func(context.Context) error { panic("private text") }, func(_ uint64, err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || err.Error() != "terminal operation panicked (string)" {
		t.Fatalf("panic result=%v", err)
	}
	if len(tracker.Pending()) != 0 {
		t.Fatal("registration leaked")
	}
}

func TestEffectDispositionSurvivesStopBeforeCallbackAcknowledgement(t *testing.T) {
	tracker := New(t.Context())
	t.Cleanup(tracker.Stop)
	release := make(chan struct{})
	callback := make(chan struct{})
	callbackRelease := make(chan struct{})
	joined := make(chan struct{})
	t.Cleanup(func() { close(callbackRelease); <-joined })
	fault := errors.New("cleanup custody retained")
	id, err := tracker.StartEffect(Reference{Description: "admit task", Command: []string{"status", "id"}}, func(context.Context) Disposition {
		<-release
		return Disposition{Err: fault, RestartRequired: true}
	}, func(uint64, error) { close(callback); <-callbackRelease; close(joined) })
	if err != nil {
		t.Fatal(err)
	}
	tracker.Stop()
	close(release)
	<-callback
	state := tracker.Snapshot()
	if len(state.Pending) != 0 || len(state.Completed) != 1 || !state.Completed[0].RestartRequired || !errors.Is(state.Completed[0].Err, fault) {
		t.Fatalf("completion disappeared before presentation: %+v", state)
	}
	state.Completed[0].Reference.Command[0] = "changed"
	if tracker.Snapshot().Completed[0].Reference.Command[0] != "status" {
		t.Fatal("completion snapshot mutated owner state")
	}
	tracker.Acknowledge(id)
	if len(tracker.Snapshot().Completed) != 0 {
		t.Fatal("presented disposition retained indefinitely")
	}
}
