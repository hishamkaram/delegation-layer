package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func transportFiniteExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-supervisor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func awaitTransportPending(t *testing.T, pending *Pending) CommandResult {
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
	executable := transportFiniteExecutable(t, "printf 'stdout\\n'; printf 'stderr\\n' >&2")
	pending := StartOwned(exec.Command(executable))
	result, err := ObserveCommand(context.Background(), pending, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || result.ExitCode != 0 || string(result.Stdout) != "stdout\n" || string(result.Stderr) != "stderr\n" {
		t.Fatalf("unexpected natural result: %+v", result)
	}
	if got := awaitTransportPending(t, pending); string(got.Stdout) != "stdout\n" {
		t.Fatalf("pending result differs: %+v", got)
	}
}

func TestObserveCommandDetachesCallerWithoutStoppingProcess(t *testing.T) {
	executable := transportFiniteExecutable(t, "sleep 0.08; printf 'finished\\n'")
	pending := StartOwned(exec.Command(executable))
	_, err := ObserveCommand(context.Background(), pending, 5*time.Millisecond)
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending != pending || !errors.Is(err, ErrInFlight) {
		t.Fatalf("expected in-flight observation, got %v", err)
	}
	result := awaitTransportPending(t, pending)
	if !result.Started || result.ExitCode != 0 || string(result.Stdout) != "finished\n" {
		t.Fatalf("detached process was not naturally reaped: %+v", result)
	}
}

func TestObserveCommandDrainsAndCapsControlOutput(t *testing.T) {
	executable := transportFiniteExecutable(t, "dd if=/dev/zero bs=1048577 count=1 2>/dev/null")
	pending := StartOwned(exec.Command(executable))
	result, err := ObserveCommand(context.Background(), pending, 2*time.Second)
	if !errors.Is(err, ErrControlLimit) {
		t.Fatalf("expected output limit, got %v", err)
	}
	if len(result.Stdout) != MaxControlBytes || !result.Started {
		t.Fatalf("output cap or start state wrong: len=%d started=%v", len(result.Stdout), result.Started)
	}
	if waited := awaitTransportPending(t, pending); len(waited.Stdout) != MaxControlBytes {
		t.Fatalf("natural result did not retain capped output: %d", len(waited.Stdout))
	}
}

func TestObserveCommandReportsStartFailureWithoutInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	pending := StartOwned(exec.Command(path))
	result, err := ObserveCommand(context.Background(), pending, time.Second)
	if err == nil || errors.Is(err, ErrInFlight) || result.Started || result.PID != 0 {
		t.Fatalf("start failure was misclassified: result=%+v err=%v", result, err)
	}
	if waited := awaitTransportPending(t, pending); waited.Started {
		t.Fatal("failed start reported as started after natural completion")
	}
}

func TestPendingResultIsCopiedAfterNaturalCompletion(t *testing.T) {
	executable := transportFiniteExecutable(t, "printf 'immutable'")
	pending := StartOwned(exec.Command(executable))
	result := awaitTransportPending(t, pending)
	result.Stdout[0] = 'X'
	again, done := pending.Result()
	if !done || string(again.Stdout) != "immutable" {
		t.Fatalf("pending output alias escaped: %+v", again)
	}
}

func TestPendingHandlesTraversesWrappedNestedJoins(t *testing.T) {
	first, second := NewPending(nil), NewPending(nil)
	firstError := &InFlightError{Pending: first}
	secondError := &InFlightError{Pending: second}
	tests := []struct {
		name string
		err  error
		want []*Pending
	}{
		{name: "nil"},
		{
			name: "single wrapped error",
			err:  fmt.Errorf("observe: %w", firstError),
			want: []*Pending{first},
		},
		{
			name: "joined errors",
			err:  errors.Join(firstError, secondError),
			want: []*Pending{first, second},
		},
		{
			name: "wrapped nested joins deduplicate pending pointers",
			err: fmt.Errorf("outer: %w", errors.Join(
				fmt.Errorf("first: %w", firstError),
				errors.Join(
					fmt.Errorf("second: %w", secondError),
					fmt.Errorf("duplicate: %w", firstError),
				),
			)),
			want: []*Pending{first, second},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := PendingHandles(test.err)
			if len(got) != len(test.want) {
				t.Fatalf("pending handle count = %d, want %d: %v", len(got), len(test.want), got)
			}
			for index, pending := range test.want {
				if got[index] != pending {
					t.Fatalf("pending handle %d = %p, want %p", index, got[index], pending)
				}
			}
		})
	}
}
