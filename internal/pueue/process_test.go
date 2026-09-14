package pueue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func finiteExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-supervisor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitPending(t *testing.T, pending *Pending) CommandResult {
	t.Helper()
	select {
	case <-pending.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("finite fake process did not exit")
	}
	result, done := pending.Result()
	if !done {
		t.Fatal("pending result was not published after done")
	}
	return result
}

func TestObserveCommandOwnsNaturalStartAndWait(t *testing.T) {
	executable := finiteExecutable(t, "printf 'stdout\\n'; printf 'stderr\\n' >&2")
	pending := startOwned(exec.Command(executable))
	result, err := observeCommand(context.Background(), pending, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || result.ExitCode != 0 || string(result.Stdout) != "stdout\n" || string(result.Stderr) != "stderr\n" {
		t.Fatalf("unexpected natural result: %+v", result)
	}
	if got := waitPending(t, pending); string(got.Stdout) != "stdout\n" {
		t.Fatalf("pending result differs: %+v", got)
	}
}

func TestObserveCommandDetachesCallerWithoutStoppingProcess(t *testing.T) {
	executable := finiteExecutable(t, "sleep 0.08; printf 'finished\\n'")
	pending := startOwned(exec.Command(executable))
	_, err := observeCommand(context.Background(), pending, 5*time.Millisecond)
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending != pending || !errors.Is(err, ErrInFlight) {
		t.Fatalf("expected in-flight observation, got %v", err)
	}
	result := waitPending(t, pending)
	if !result.Started || result.ExitCode != 0 || string(result.Stdout) != "finished\n" {
		t.Fatalf("detached process was not naturally reaped: %+v", result)
	}
}

func TestObserveCommandDrainsAndCapsControlOutput(t *testing.T) {
	executable := finiteExecutable(t, "dd if=/dev/zero bs=1048577 count=1 2>/dev/null")
	pending := startOwned(exec.Command(executable))
	result, err := observeCommand(context.Background(), pending, 2*time.Second)
	if !errors.Is(err, ErrControlLimit) {
		t.Fatalf("expected output limit, got %v", err)
	}
	if len(result.Stdout) != MaxControlBytes || !result.Started {
		t.Fatalf("output cap or start state wrong: len=%d started=%v", len(result.Stdout), result.Started)
	}
	if waited := waitPending(t, pending); len(waited.Stdout) != MaxControlBytes {
		t.Fatalf("natural result did not retain capped output: %d", len(waited.Stdout))
	}
	if acknowledged := commandAcknowledgment(result, err); acknowledged != nil {
		t.Fatalf("control output overflow was classified as acknowledgment: %v", *acknowledged)
	}
}

func TestObserveCommandReportsStartFailureWithoutInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	pending := startOwned(exec.Command(path))
	result, err := observeCommand(context.Background(), pending, time.Second)
	if err == nil || errors.Is(err, ErrInFlight) || result.Started || result.PID != 0 {
		t.Fatalf("start failure was misclassified: result=%+v err=%v", result, err)
	}
	if acknowledged := commandAcknowledgment(result, err); acknowledged != nil {
		t.Fatalf("start failure was classified as acknowledgment: %v", *acknowledged)
	}
	if waited := waitPending(t, pending); waited.Started {
		t.Fatal("failed start reported as started after natural completion")
	}
}

func TestCommandAcknowledgmentAcceptsOnlyNormalCompletedRefusal(t *testing.T) {
	executable := finiteExecutable(t, "exit 7")
	result, err := observeCommand(context.Background(), startOwned(exec.Command(executable)), time.Second)
	if err == nil {
		t.Fatal("nonzero fake command unexpectedly succeeded")
	}
	acknowledged := commandAcknowledgment(result, err)
	if acknowledged == nil || *acknowledged {
		t.Fatalf("normal nonzero command was not classified as refusal: %+v %v", result, err)
	}
	if acknowledged = commandAcknowledgment(CommandResult{Started: true, ExitCode: 0}, errors.New("wait infrastructure failure")); acknowledged != nil {
		t.Fatalf("wait infrastructure failure was classified as acknowledgment: %v", *acknowledged)
	}
}

func TestPendingResultIsCopiedAfterNaturalCompletion(t *testing.T) {
	executable := finiteExecutable(t, "printf 'immutable'")
	pending := startOwned(exec.Command(executable))
	result := waitPending(t, pending)
	result.Stdout[0] = 'X'
	again, done := pending.Result()
	if !done || string(again.Stdout) != "immutable" {
		t.Fatalf("pending output alias escaped: %+v", again)
	}
}
