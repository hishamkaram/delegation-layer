package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// terminalEventClockSupervisor holds only prerequisites completed before a
// synctest bubble starts: the store, fake executables, the endpoint listener,
// the prepared client and the command recorders. No command remains
// outstanding. Operations, jobs, lifetimes, receipts, queue statuses, tickets
// and every observation context are created inside the bubble, so every
// admission deadline fact shares the bubble clock.
type terminalEventClockSupervisor struct {
	store                 *taskdir.Store
	client                *pueue.Client
	commands              *terminalEventClockCommands
	launches              *atomic.Int32
	statusCalls           *atomic.Int32
	unfilteredStatusCalls *atomic.Int32
}

// terminalEventClockCommands records every command event. Its entry gate is
// nil except while a test inside the bubble installs one, so only bubble
// command goroutines can wait on the gate's bubble channels.
type terminalEventClockCommands struct {
	recorded pendingPublicationCommands
	gate     atomic.Pointer[terminalEventEntryGate]
}

func (commands *terminalEventClockCommands) observe(event pueue.CommandEvent) {
	commands.recorded.observe(event)
	if gate := commands.gate.Load(); gate != nil && event.Stage == "entry" && unfilteredTerminalEventStatus(event) {
		gate.hold()
	}
}

func (commands *terminalEventClockCommands) since(baseline int) []pueue.CommandEvent {
	return commands.recorded.snapshot()[baseline:]
}

// installEntryGate returns a gate and its idempotent release, which also
// clears the gate. Callers defer the release inside the bubble body so it runs
// before the bubble joins callers and closes the client.
func (commands *terminalEventClockCommands) installEntryGate() (*terminalEventEntryGate, func()) {
	gate := &terminalEventEntryGate{held: make(chan struct{}), release: make(chan struct{})}
	commands.gate.Store(gate)
	return gate, func() {
		commands.gate.Store(nil)
		gate.releaseOnce.Do(func() { close(gate.release) })
	}
}

// terminalEventEntryGate holds the first unfiltered status command at its
// entry event, before its process starts. The holder waits on a bubble
// channel, which is durably blocked, so the bubble clock advances while the
// command's owner retains its leases.
type terminalEventEntryGate struct {
	claimed     atomic.Bool
	held        chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (gate *terminalEventEntryGate) hold() {
	if !gate.claimed.CompareAndSwap(false, true) {
		return
	}
	close(gate.held)
	<-gate.release
}

// terminalEventClockScope is the bubble-owned lifetime of a test body. Every
// goroutine that calls admission or maintenance is started through callers so
// the bubble cancels and joins it before the client closes and before the
// operation journals registered with t.Cleanup close.
type terminalEventClockScope struct {
	ctx     context.Context
	callers sync.WaitGroup
}

func newTerminalEventClockSupervisor(t *testing.T) terminalEventClockSupervisor {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	launches := &atomic.Int32{}
	commands := &terminalEventClockCommands{}
	// The fixture's idempotent outside Close owns cleanup when setup fails
	// before the bubble starts; afterwards it observes an already closed client.
	client, statusCalls, unfilteredStatusCalls := admissionfixture.NewTerminalEventInspectionSupervisorWithObserver(
		t, store, TerminalEvidence{}, func(context.Context, string, string) error {
			launches.Add(1)
			return nil
		}, commands.observe,
	)
	return terminalEventClockSupervisor{
		store: store, client: client, commands: commands, launches: launches,
		statusCalls: statusCalls, unfilteredStatusCalls: unfilteredStatusCalls,
	}
}

// runTerminalEventClockBubble runs body on one synctest clock. Deferred calls
// run on success, early return and Fatal: first body's own defers, which
// release any forced gate, then caller cancellation and join, then client
// Close, and only then the t.Cleanup functions registered inside the bubble.
func runTerminalEventClockBubble(t *testing.T, supervisor terminalEventClockSupervisor, body func(*testing.T, *terminalEventClockScope)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		defer func() {
			if err := supervisor.client.Close(); err != nil {
				t.Error(err)
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		scope := &terminalEventClockScope{ctx: ctx}
		defer func() {
			cancel()
			scope.callers.Wait()
		}()
		body(t, scope)
	})
}

type terminalEventClockInspection struct {
	operation *inspection.Operation
	job       pueue.Job
}

func newTerminalEventClockInspection(t *testing.T, supervisor terminalEventClockSupervisor, index int, registered bool) terminalEventClockInspection {
	t.Helper()
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperationForTask(
		t, supervisor.store, supervisor.client, inspection.ResultEligible, true, fmt.Sprintf("%032x", index+1), int64(index+7),
	)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	admissionfixture.RequireAppTest(t, operation.RecordReceiptContext(t.Context(), job.ID))
	if registered {
		admissionfixture.RequireAppTest(t, supervisor.client.RegisterPrivateTerminalEvent(t.Context(), job.ID))
	}
	return terminalEventClockInspection{operation: operation, job: job}
}

type terminalEventClockBaseline struct {
	commands int
	journal  publicationMaintenanceJournal
}

func requireTerminalEventClockBaseline(t *testing.T, supervisor terminalEventClockSupervisor, ids ...int64) terminalEventClockBaseline {
	t.Helper()
	for _, id := range ids {
		if _, found, err := supervisor.client.ReadPrivateTerminalEvent(t.Context(), id); err != nil || found {
			t.Fatalf("candidate event %d before observation found=%t err=%v", id, found, err)
		}
	}
	if calls, unfiltered, launches := supervisor.statusCalls.Load(), supervisor.unfilteredStatusCalls.Load(), supervisor.launches.Load(); calls != 0 || unfiltered != 0 || launches != 0 {
		t.Fatalf("prerequisites made status_calls=%d unfiltered_status_calls=%d launches=%d, want none", calls, unfiltered, launches)
	}
	baseline := terminalEventClockBaseline{commands: len(supervisor.commands.recorded.snapshot())}
	if _, err := os.Stat(filepath.Join(supervisor.store.Root, ".supervisor", "queue-maintenance.json")); err == nil {
		baseline.journal = readPublicationJournal(t, supervisor.store.Root)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return baseline
}

func requireTerminalEventClockUnexpired(t *testing.T, operation *inspection.Operation) {
	t.Helper()
	if operation.Expired(time.Now()) {
		t.Fatalf("admission returned after its deadline %s on the bubble clock", operation.Deadline())
	}
}

// requireTerminalEventClockCandidates runs before canonical maintenance. Every
// row took the strict candidate branch: one completed admission
// reconciliation, then one completed candidate revalidation, a published
// event, and no full-recovery wake.
func requireTerminalEventClockCandidates(t *testing.T, ctx context.Context, supervisor terminalEventClockSupervisor, baseline terminalEventClockBaseline, ids ...int64) {
	t.Helper()
	if launches := supervisor.launches.Load(); launches != int32(len(ids)) {
		t.Fatalf("maintenance launch requests before canonical maintenance=%d, want %d", launches, len(ids))
	}
	for _, id := range ids {
		if event, found, err := supervisor.client.ReadPrivateTerminalEvent(ctx, id); err != nil || !found || !event.Succeeded {
			t.Fatalf("candidate event %d after admission=%+v found=%t err=%v", id, event, found, err)
		}
	}
	journal := readPublicationJournal(t, supervisor.store.Root)
	if journal.Requested != baseline.journal.Requested+uint64(len(ids)) || journal.FullRequested != baseline.journal.FullRequested {
		t.Fatalf("candidate wake journal=%+v baseline=%+v, want %d candidate requests and no full recovery", journal, baseline.journal, len(ids))
	}
	events := supervisor.commands.since(baseline.commands)
	filtered, unfiltered := terminalEventStatusEntries(events)
	if len(filtered) != 2*len(ids) || len(unfiltered) != 0 {
		t.Fatalf("pre-maintenance status entries filtered=%d unfiltered=%d, want %d and 0: %+v", len(filtered), len(unfiltered), 2*len(ids), events)
	}
	byLabel := make(map[string][]pueue.CommandEvent, len(ids))
	for _, entry := range filtered {
		label := entry.Argv[slices.IndexFunc(entry.Argv, admissionfixture.TerminalMaintenanceLabelFilter)]
		byLabel[label] = append(byLabel[label], entry)
	}
	for label, entries := range byLabel {
		if len(entries) != 2 {
			t.Fatalf("label %s has %d filtered status entries, want admission and candidate revalidation", label, len(entries))
		}
		admission := requireTerminalEventCommandCompleted(t, events, entries[0])
		candidate := slices.IndexFunc(events, func(event pueue.CommandEvent) bool {
			return event.CommandID == entries[1].CommandID && event.Stage == "entry"
		})
		if candidate <= admission {
			t.Fatalf("candidate revalidation entry=%d did not follow admission completion=%d for %s", candidate, admission, label)
		}
		requireTerminalEventCommandCompleted(t, events, entries[1])
	}
}

// requireTerminalEventClockCleanup proves the canonical maintenance owner made
// exactly the wanted status calls, every observed command started and exited
// 0, the wake journal settled, and each row's event and ticket retired.
func requireTerminalEventClockCleanup(t *testing.T, ctx context.Context, supervisor terminalEventClockSupervisor, baseline terminalEventClockBaseline, filteredCalls, unfilteredCalls int, ids ...int64) {
	t.Helper()
	if total, unfiltered := supervisor.statusCalls.Load(), supervisor.unfilteredStatusCalls.Load(); int(total) != filteredCalls+unfilteredCalls || int(unfiltered) != unfilteredCalls {
		t.Fatalf("status_calls=%d unfiltered_status_calls=%d, want %d with %d unfiltered", total, unfiltered, filteredCalls+unfilteredCalls, unfilteredCalls)
	}
	events := supervisor.commands.since(baseline.commands)
	filtered, unfiltered := terminalEventStatusEntries(events)
	if len(filtered) != filteredCalls || len(unfiltered) != unfilteredCalls {
		t.Fatalf("status entries filtered=%d unfiltered=%d, want %d and %d: %+v", len(filtered), len(unfiltered), filteredCalls, unfilteredCalls, events)
	}
	for _, event := range events {
		if event.Stage == "entry" {
			requireTerminalEventCommandCompleted(t, events, event)
		}
	}
	requirePublicationJournalSettled(t, supervisor.store.Root)
	for _, id := range ids {
		admissionfixture.WaitForPrivateTerminalEventRetirement(t, ctx, supervisor.client, id)
		for _, directory := range []string{"terminal-event-tickets", "terminal-events"} {
			path := filepath.Join(supervisor.store.Root, ".supervisor", directory, strconv.FormatInt(id, 10)+".json")
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("maintenance did not retire %s record %d: %v", directory, id, err)
			}
		}
	}
}

func unfilteredTerminalEventStatus(event pueue.CommandEvent) bool {
	return slices.Contains(event.Argv, "status") && !slices.ContainsFunc(event.Argv, admissionfixture.TerminalMaintenanceLabelFilter)
}

func terminalEventStatusEntries(events []pueue.CommandEvent) (filtered, unfiltered []pueue.CommandEvent) {
	for _, event := range events {
		if event.Stage != "entry" || !slices.Contains(event.Argv, "status") {
			continue
		}
		if unfilteredTerminalEventStatus(event) {
			unfiltered = append(unfiltered, event)
		} else {
			filtered = append(filtered, event)
		}
	}
	return filtered, unfiltered
}

// requireTerminalEventCommandCompleted proves the command reached its exact
// native Wait: a started PID and a zero-exit completion. It returns the
// completion index.
func requireTerminalEventCommandCompleted(t *testing.T, events []pueue.CommandEvent, entry pueue.CommandEvent) int {
	t.Helper()
	started := slices.ContainsFunc(events, func(event pueue.CommandEvent) bool {
		return event.CommandID == entry.CommandID && event.Stage == "started" && event.PID > 0
	})
	completed := slices.IndexFunc(events, func(event pueue.CommandEvent) bool {
		return event.CommandID == entry.CommandID && event.Stage == "completed"
	})
	if entry.CommandID == "" || !started || completed < 0 || events[completed].ExitCode != 0 {
		t.Fatalf("command %q argv=%v did not start and complete with exit 0: %+v", entry.CommandID, entry.Argv, events)
	}
	return completed
}
