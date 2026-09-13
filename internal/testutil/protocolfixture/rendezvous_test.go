package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rendezvousObservation struct {
	pid      int
	complete bool
	err      error
}

func TestRendezvousWaitsForCompleteReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticks := make(chan time.Time)
	observed := make(chan rendezvousObservation, 1)
	done := make(chan rendezvousObservation, 1)
	read := func() (int, bool, error) {
		pid, complete, err := readRendezvous(path, "waiting", 123)
		observed <- rendezvousObservation{pid: pid, complete: complete, err: err}
		return pid, complete, err
	}
	go func() {
		pid, err := pollRendezvous(ctx, read, ticks)
		done <- rendezvousObservation{pid: pid, err: err}
	}()
	// Advance only after the previous read was observed. No scheduler delay or
	// fixed sleep is used to manufacture an empty/partial-file visibility window.
	expectPendingReceipt(t, ctx, observed, done)
	for _, data := range []string{"", "123:", "123:waiting"} {
		writeTestFile(t, path, []byte(data))
		advanceReceiptPoll(t, ctx, ticks, done)
		expectPendingReceipt(t, ctx, observed, done)
	}
	writeTestFile(t, path, []byte("123:waiting\n"))
	advanceReceiptPoll(t, ctx, ticks, done)
	select {
	case result := <-done:
		if result.err != nil || result.pid != 123 {
			t.Fatalf("complete receipt: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("complete receipt never acknowledged")
	}
}

func expectPendingReceipt(t *testing.T, ctx context.Context, observed, done <-chan rendezvousObservation) {
	t.Helper()
	select {
	case result := <-observed:
		if result.err != nil || result.complete || result.pid != 0 {
			t.Fatalf("incomplete receipt accepted: %+v", result)
		}
	case result := <-done:
		t.Fatalf("wait ended before complete receipt: %+v", result)
	case <-ctx.Done():
		t.Fatal("receipt read did not occur")
	}
}

func advanceReceiptPoll(t *testing.T, ctx context.Context, ticks chan<- time.Time, done <-chan rendezvousObservation) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case result := <-done:
		t.Fatalf("wait ended before final receipt: %+v", result)
	case <-ctx.Done():
		t.Fatal("receipt waiter stopped polling")
	}
}

func TestRendezvousRejectsMalformedCompletedReceipt(t *testing.T) {
	for _, data := range []string{"\n", "bad:waiting\n", "0:waiting\n", "+123:waiting\n", "123:wrong-label\n", "124:waiting\n", "123:waiting\nextra", "123:waiting\n123:waiting\n", strings.Repeat("x", maxRendezvousBytes+1)} {
		t.Run(data[:min(len(data), 30)], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt")
			writeTestFile(t, path, []byte(data))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			read := func() (int, bool, error) { return readRendezvous(path, "waiting", 123) }
			pid, err := pollRendezvous(ctx, read, nil)
			if pid != 0 || err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("malformed final receipt was retried/accepted: pid=%d err=%v", pid, err)
			}
		})
	}
}

func TestRendezvousIncompleteReceiptHasFiniteDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt")
	writeTestFile(t, path, []byte("123:waiting"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read := func() (int, bool, error) { return readRendezvous(path, "waiting", 123) }
	pid, err := pollRendezvous(ctx, read, nil)
	if pid != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("incomplete receipt escaped deadline: pid=%d err=%v", pid, err)
	}
}

func TestRendezvousRejectsNonregularAndPropagatesRecorderError(t *testing.T) {
	dir := t.TempDir()
	if _, complete, err := readRendezvous(dir, "waiting", 0); complete || err == nil {
		t.Fatal("directory treated as receipt")
	}
	if err := signalRendezvous(dir, "waiting"); err == nil {
		t.Fatal("recorder failure ignored")
	}
	path := filepath.Join(dir, "receipt")
	if err := signalRendezvous(path, "waiting"); err != nil {
		t.Fatal(err)
	}
	pid, complete, err := readRendezvous(path, "waiting", os.Getpid())
	if err != nil || !complete || pid != os.Getpid() {
		t.Fatalf("real recorder control: %d %t %v", pid, complete, err)
	}
}
