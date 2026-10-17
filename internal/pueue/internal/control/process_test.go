package control

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

func TestStatusObservationErrorMarksOnlyTransientCommandFailures(t *testing.T) {
	executable := finiteExecutable(t, "exit 23")
	_, exitErr := observeCommand(context.Background(), startOwned(exec.Command(executable)), time.Second)
	if classified := statusObservationError(exitErr); !errors.Is(classified, ErrStatusOperational) || !errors.Is(classified, ErrStatusTransient) {
		t.Fatalf("nonzero status was not transient: %v", classified)
	}
	malformed := errors.Join(ErrUnknown, errors.New("invalid status schema"))
	if classified := statusObservationError(malformed); !errors.Is(classified, ErrStatusOperational) || errors.Is(classified, ErrStatusTransient) {
		t.Fatalf("malformed status became transient: %v", classified)
	}
	limited := errors.Join(exitErr, ErrControlLimit)
	if classified := statusObservationError(limited); errors.Is(classified, ErrStatusTransient) {
		t.Fatalf("status output limit became transient: %v", classified)
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
