package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestSettlementPendingObservationRetainsCommandAndLease(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		name := "prepare"
		if confirm {
			name = "confirm"
		}
		t.Run(name, func(t *testing.T) {
			fixture, entered, release := prepareSettlementPendingFixture(t)
			receipt, err := fixture.td.ReadSupervisorReceipt()
			requireSettlementTest(t, err)
			queue := task.OperatorDispositionQueue{NumericTaskID: receipt.NumericTaskID, Label: receipt.Label, Ended: true}
			_, challenge, err := fixture.td.PrepareOperatorDispositionContext(t.Context(), queue, lifetime.NativeObservation{})
			requireSettlementTest(t, err)
			args := Arguments{Root: fixture.root, TaskID: fixture.td.TaskID, SettlementPrepare: !confirm}
			if confirm {
				args.SettlementConfirm = challenge
			}
			deps := fixture.dependencies()
			// Setup clients join their work. The ordinary CLI transfers Pending.
			deps.SupervisorOptions.WaitForPendingClose = false
			deps.SupervisorOptions.ObservationTimeout = 2 * time.Second
			started := make(chan pueue.CommandEvent, 1)
			deps.SupervisorOptions.Observer = func(event pueue.CommandEvent) {
				if event.Stage == "started" && slices.Contains(event.Argv, "label="+receipt.Label) {
					started <- event
				}
			}
			call := startSettlementPendingCall(t, args, deps, release)
			awaitSettlementPendingGate(t, entered, call.done)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var event pueue.CommandEvent
			select {
			case event = <-started:
			case <-ctx.Done():
				t.Fatal("label query did not publish its native Start")
			}
			select {
			case <-call.done:
			case <-ctx.Done():
				t.Fatal("settlement did not return within the finite observation window while its label query remained owned")
			}
			pending := requireSettlementPendingResult(t, fixture, call.result, event)
			requireSettlementBootstrapLease(t, fixture.root, true)
			requireSettlementTest(t, release())
			joinSettlementPending(t, call.result.err)
			assertInspectionWorkerTimeoutResult(t, pending)
			requireSettlementBootstrapLease(t, fixture.root, false)
			requireNoPendingSettlementSidecar(t, fixture)
		})
	}
}

// Only the task-specific status blocks. The saved-private readiness probe and
// real store preparation finish before the native command enters this FIFO.
func prepareSettlementPendingFixture(t *testing.T) (*historicalSupervisorFixture, string, func() error) {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	base := filepath.Dir(fixture.root)
	fifo, enabled, entered := filepath.Join(base, "label-release"), filepath.Join(base, "label-hold"), filepath.Join(base, "label-entered")
	requireSettlementTest(t, unix.Mkfifo(fifo, 0o600))
	gate, err := os.OpenFile(fifo, os.O_RDWR, 0)
	requireSettlementTest(t, err)
	release := sync.OnceValue(func() error {
		_, writeErr := gate.WriteString("release\n")
		return errors.Join(writeErr, gate.Close())
	})
	t.Cleanup(func() { requireSettlementTest(t, release()) })
	prefix := fmt.Sprintf(`#!/bin/sh
set -eu
if [ -f '%s' ]; then
  for argument in "$@"; do
    case "$argument" in
      label=*)
        printf 'started\n' > '%s'
        IFS= read -r release < '%s'
        break
        ;;
    esac
  done
fi
`, enabled, entered, fifo)
	pidPath := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.clientA, prefix+historicalSupervisorClientScript("A", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	requireSettlementTest(t, os.WriteFile(enabled, nil, 0o600))
	return fixture, entered, release
}

type settlementPendingCall struct {
	done   chan struct{}
	result commandResult
}

func startSettlementPendingCall(t *testing.T, args Arguments, deps Dependencies, release func() error) *settlementPendingCall {
	t.Helper()
	call := &settlementPendingCall{done: make(chan struct{})}
	t.Cleanup(func() {
		requireSettlementTest(t, release())
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		waitInspectionWorkerTimeoutSignal(t, ctx, call.done, "settlement caller cleanup")
		joinSettlementPending(t, call.result.err)
	})
	go func() {
		call.result = settleContext(t.Context(), args, deps)
		close(call.done)
	}()
	return call
}

func awaitSettlementPendingGate(t *testing.T, entered string, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(entered); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-done:
			t.Fatal("settlement returned before the exact label command entered its gate")
		case <-ctx.Done():
			t.Fatal("exact label command did not enter its gate")
		case <-ticker.C:
		}
	}
}

func requireSettlementPendingResult(t *testing.T, fixture *historicalSupervisorFixture, result commandResult, event pueue.CommandEvent) *pueue.Pending {
	t.Helper()
	var inFlight *pueue.InFlightError
	if result.code != 1 || result.response.Settlement != nil || !errors.As(result.err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("settlement lost its owned pending query: %+v", result)
	}
	select {
	case <-inFlight.Pending.Done():
		t.Fatal("settlement completed the gated native command")
	default:
	}
	ref, err := lifetime.ObserveProcess(event.PID)
	requireSettlementTest(t, err)
	present, err := lifetime.ProcessPresent(ref)
	if err != nil || !present {
		t.Fatalf("pending command lost its native process: present=%t err=%v", present, err)
	}
	requireNoPendingSettlementSidecar(t, fixture)
	return inFlight.Pending
}

func requireNoPendingSettlementSidecar(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(fixture.td.Dir, "operator-disposition.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending observation created a settlement sidecar: %v", err)
	}
}

func requireSettlementBootstrapLease(t *testing.T, root string, held bool) {
	t.Helper()
	timeout := 5 * time.Second
	if held {
		timeout = 100 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	entered := false
	err := pueue.WithExistingBootstrapLockContext(ctx, root, func(context.Context) error {
		entered = true
		return nil
	})
	if held {
		if entered || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pending command released bootstrap EX: entered=%t err=%v", entered, err)
		}
	} else if err != nil || !entered {
		t.Fatalf("joined command retained bootstrap EX: entered=%t err=%v", entered, err)
	}
}

func joinSettlementPending(t *testing.T, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancel()
	var inFlight *pueue.InFlightError
	if errors.As(err, &inFlight) && inFlight.Pending != nil {
		waitInspectionWorkerTimeoutSignal(t, ctx, inFlight.Pending.Done(), "settlement command Wait")
	}
	var cleanup *pueue.BootstrapCleanupError
	if errors.As(err, &cleanup) {
		waitInspectionWorkerTimeoutSignal(t, ctx, cleanup.Done(), "settlement bootstrap cleanup")
		if cleanupErr := cleanup.Close(); cleanupErr != nil {
			t.Errorf("settlement bootstrap cleanup: %v", cleanupErr)
		}
	}
}
