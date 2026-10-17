package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type pendingPublicationCommands struct {
	mu     sync.Mutex
	events []pueue.CommandEvent
}

func (commands *pendingPublicationCommands) observe(event pueue.CommandEvent) {
	commands.mu.Lock()
	defer commands.mu.Unlock()
	event.Argv = slices.Clone(event.Argv)
	commands.events = append(commands.events, event)
}

func (commands *pendingPublicationCommands) snapshot() []pueue.CommandEvent {
	commands.mu.Lock()
	defer commands.mu.Unlock()
	return slices.Clone(commands.events)
}

type pendingPublicationFixture struct {
	store        *taskdir.Store
	client       *pueue.Client
	observations []pueue.InspectionObservation
	commands     *pendingPublicationCommands
	launches     *atomic.Int32
	block        string
	release      string
}

func newPendingPublicationFixture(t *testing.T) pendingPublicationFixture {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	gate := t.TempDir()
	fixture := pendingPublicationFixture{
		store: store, commands: &pendingPublicationCommands{}, launches: &atomic.Int32{},
		block: filepath.Join(gate, "block"), release: filepath.Join(gate, "release"),
	}
	fixture.client, _, _ = admissionfixture.NewTerminalEventInspectionSupervisorWithObserver(
		t, store, TerminalEvidence{}, func(context.Context, string, string) error {
			fixture.launches.Add(1)
			return nil
		}, fixture.commands.observe,
		"FAKE_BLOCK_CLEANUP_STATUS="+fixture.block, "FAKE_RELEASE_CLEANUP_STATUS="+fixture.release,
	)
	t.Cleanup(func() { releasePublicationCommand(t, fixture.release) })
	jobs := make([]pueue.Job, 0, 2)
	operations := make([]*inspection.Operation, 0, 2)
	for index := range 2 {
		operation, job := admissionfixture.NewTerminalInspectionCleanupOperationForTask(t, store, fixture.client, inspection.ResultEligible, true, fmt.Sprintf("%032x", index+1), int64(index+7))
		admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
		if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
		if err := fixture.client.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
		jobs, operations = append(jobs, job), append(operations, operation)
	}
	writeTerminalEventStatuses(t, store, jobs)
	for _, operation := range operations {
		identity, err := InspectionIdentityContext(t.Context(), operation)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := fixture.client.ReconcileInspection(t.Context(), identity)
		if err != nil || !observation.Matched {
			t.Fatalf("initial exact inspection observation: %+v, %v", observation, err)
		}
		fixture.observations = append(fixture.observations, observation)
	}
	return fixture
}

func TestTerminalObservationPendingCannotLoseInspectionCleanupCandidate(t *testing.T) {
	fixture := newPendingPublicationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	if !supervisorcore.RequestInspectionQueueMaintenance(ctx, fixture.client, fixture.observations[0]) {
		t.Fatal("first observation did not hand off maintenance")
	}
	if err := os.WriteFile(fixture.block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !supervisorcore.RequestInspectionQueueMaintenance(ctx, fixture.client, fixture.observations[1]) {
		t.Fatal("pending publication did not hand off full recovery")
	}
	started := requireBlockedPublication(t, ctx, fixture)
	releasePublicationCommand(t, fixture.release)
	runInspectionMaintenanceBatch(t, ctx, fixture.client, fixture.launches.Load(), 2)
	if err := fixture.client.Close(); err != nil {
		t.Fatal(err)
	}
	requirePublicationCommandCompleted(t, fixture.commands.snapshot(), started)
	removals := filepath.Join(fixture.store.Root, ".supervisor", "terminal-event-removals")
	data, err := os.ReadFile(removals)
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(data))
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"7", "8"}) {
		t.Errorf("settled publication recovery removed %v, want [7 8]", ids)
	}
	requirePublicationJournalSettled(t, fixture.store.Root)
	for _, directory := range []string{"terminal-event-tickets", "terminal-events"} {
		for _, id := range []int64{7, 8} {
			path := filepath.Join(fixture.store.Root, ".supervisor", directory, strconv.FormatInt(id, 10)+".json")
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("full recovery did not retire %s record %d: %v", directory, id, err)
			}
		}
	}
}

func releasePublicationCommand(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Error(err)
	}
}

func requireBlockedPublication(t *testing.T, ctx context.Context, fixture pendingPublicationFixture) pueue.CommandEvent {
	t.Helper()
	admissionfixture.WaitForTestPath(t, filepath.Join(fixture.store.Root, ".supervisor", "terminal-event-status-blocked"))
	for index, want := range []bool{true, false} {
		_, found, err := fixture.client.ReadPrivateTerminalEvent(ctx, int64(index+7))
		if err != nil || found != want {
			t.Fatalf("pre-recovery event %d: found=%t want=%t err=%v", index+7, found, want, err)
		}
	}
	journal := readPublicationJournal(t, fixture.store.Root)
	if journal.FullRequested == 0 || journal.Completed >= journal.FullRequested {
		t.Fatalf("failed publication lost full recovery wake: %+v", journal)
	}
	label := *fixture.observations[1].Job.Label
	for _, event := range slices.Backward(fixture.commands.snapshot()) {
		if event.Stage == "started" && slices.ContainsFunc(event.Argv, func(arg string) bool { return strings.Contains(arg, label) }) {
			if event.CommandID == "" || event.PID <= 0 {
				t.Fatalf("blocked publication has no owned started command identity: %+v", event)
			}
			if _, completed := publicationCompletion(fixture.commands.snapshot(), event.CommandID); completed {
				t.Fatal("publication command completed before its release gate")
			}
			return event
		}
	}
	t.Fatal("blocked publication did not expose its actual started command")
	return pueue.CommandEvent{}
}

func requirePublicationCommandCompleted(t *testing.T, events []pueue.CommandEvent, started pueue.CommandEvent) {
	t.Helper()
	if event, found := publicationCompletion(events, started.CommandID); !found || event.ExitCode != 0 {
		t.Fatalf("blocked command did not settle naturally: id=%s pid=%d events=%+v", started.CommandID, started.PID, events)
	}
}

func publicationCompletion(events []pueue.CommandEvent, id string) (pueue.CommandEvent, bool) {
	for _, event := range events {
		if event.CommandID == id && event.Stage == "completed" {
			return event, true
		}
	}
	return pueue.CommandEvent{}, false
}

type publicationMaintenanceJournal struct {
	Requested     uint64 `json:"requested"`
	Attempted     uint64 `json:"attempted"`
	Completed     uint64 `json:"completed"`
	FullRequested uint64 `json:"full_requested"`
	FullCompleted uint64 `json:"full_completed"`
}

func readPublicationJournal(t *testing.T, root string) publicationMaintenanceJournal {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".supervisor", "queue-maintenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal publicationMaintenanceJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	return journal
}

func requirePublicationJournalSettled(t *testing.T, root string) {
	t.Helper()
	journal := readPublicationJournal(t, root)
	if journal.Requested == 0 || journal.Attempted != journal.Requested || journal.Completed != journal.Requested || journal.FullRequested != journal.FullCompleted {
		t.Errorf("recovery consumed its wake without settling: %+v", journal)
	}
}
