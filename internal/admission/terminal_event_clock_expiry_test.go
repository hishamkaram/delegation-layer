package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

// TestTerminalEventCandidateExpiresBeforeStatusWhileMaintenanceHoldsWriterIntent
// holds the canonical maintenance owner's first cleanup snapshot at command
// entry, where the owner keeps the queue writer intent. Admission's exact
// observation needs only the supervisor lease and completes, but candidate
// publication waits for the writer intent, so its unchanged one-second
// watchdog expires before any candidate status starts. After release, full
// recovery must authorize and remove the row through the same owner.
func TestTerminalEventCandidateExpiresBeforeStatusWhileMaintenanceHoldsWriterIntent(t *testing.T) {
	supervisor := newTerminalEventClockSupervisor(t)
	runTerminalEventClockBubble(t, supervisor, func(t *testing.T, scope *terminalEventClockScope) {
		probe := newTerminalEventClockInspection(t, supervisor, 0, true)
		writeTerminalEventStatuses(t, supervisor.store, []pueue.Job{probe.job})
		baseline := requireTerminalEventClockBaseline(t, supervisor, probe.job.ID)
		started := time.Now()
		ctx, cancel := context.WithTimeout(scope.ctx, 2*time.Second)
		defer cancel()
		gate, releaseGate := supervisor.commands.installEntryGate()
		defer releaseGate()
		maintenance := startHeldTerminalEventMaintenance(t, ctx, supervisor, scope, gate)
		facts, err := awaitInspectionWithSubmissionState(ctx, probe.operation, supervisor.client, inspectionSubmissionState{
			terminalEventRegistrationKnown: true,
			terminalEventRegistered:        true,
		})
		if err != nil || string(facts) != "{\"eligible\":true}\n" {
			t.Fatalf("admission with expired candidate facts=%s err=%v", facts, err)
		}
		if elapsed := time.Since(started); elapsed < supervisorcore.InspectionStatusWatchdog {
			t.Fatalf("admission returned at bubble clock %s, before the %s candidate watchdog", elapsed, supervisorcore.InspectionStatusWatchdog)
		}
		requireTerminalEventClockUnexpired(t, probe.operation)
		requireExpiredTerminalEventCandidate(t, ctx, supervisor, baseline, probe.job.ID)
		releaseGate()
		select {
		case ran := <-maintenance:
			if !ran {
				t.Fatal("persisted maintenance did not start")
			}
		case <-ctx.Done():
			t.Fatalf("held maintenance owner did not finish: %v", ctx.Err())
		}
		admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, ctx, supervisor.client)
		removed, readErr := os.ReadFile(filepath.Join(supervisor.store.Root, ".supervisor", "terminal-event-removals"))
		if readErr != nil || string(removed) != "7\n" {
			t.Fatalf("full recovery removal contents=%q err=%v, want %q", removed, readErr, "7\n")
		}
		// Admission's exact status, then the held cleanup snapshot and full
		// recovery's authorization/revalidation pair.
		requireTerminalEventClockCleanup(t, ctx, supervisor, baseline, 1, 3, probe.job.ID)
	})
}

// startHeldTerminalEventMaintenance publishes one candidate-scoped wake and
// runs the canonical owner as a bubble caller until its first cleanup snapshot
// reaches the entry gate. The owner then holds the queue writer intent and
// has not started a process.
func startHeldTerminalEventMaintenance(t *testing.T, ctx context.Context, supervisor terminalEventClockSupervisor, scope *terminalEventClockScope, gate *terminalEventEntryGate) <-chan bool {
	t.Helper()
	admissionfixture.RequireAppTest(t, pueue.RequestPrivateCandidateQueueMaintenance(ctx, supervisor.store.Root))
	started := make(chan bool, 1)
	scope.callers.Go(func() { started <- supervisor.client.RunRequestedPrivateQueueMaintenance(ctx) })
	select {
	case <-gate.held:
		return started
	case ran := <-started:
		t.Fatalf("maintenance returned started=%t before its cleanup snapshot reached the entry gate", ran)
	case <-ctx.Done():
		t.Fatalf("maintenance cleanup snapshot did not reach the entry gate: %v", ctx.Err())
	}
	return nil
}

// requireExpiredTerminalEventCandidate proves the candidate expired before
// any candidate command entry: no event was published, only admission's exact
// status ran, the held cleanup snapshot has not started, and admission
// published one unattempted full-recovery wake after the held candidate wake.
func requireExpiredTerminalEventCandidate(t *testing.T, ctx context.Context, supervisor terminalEventClockSupervisor, baseline terminalEventClockBaseline, id int64) {
	t.Helper()
	if launches := supervisor.launches.Load(); launches != 1 {
		t.Fatalf("maintenance launch requests after expired candidate=%d, want 1", launches)
	}
	if event, found, err := supervisor.client.ReadPrivateTerminalEvent(ctx, id); err != nil || found {
		t.Fatalf("expired candidate published event=%+v found=%t err=%v", event, found, err)
	}
	journal := readPublicationJournal(t, supervisor.store.Root)
	if journal.Requested != baseline.journal.Requested+2 || journal.FullRequested != journal.Requested || journal.Attempted != baseline.journal.Attempted {
		t.Fatalf("expired candidate journal=%+v baseline=%+v, want held candidate wake then one unattempted full recovery", journal, baseline.journal)
	}
	events := supervisor.commands.since(baseline.commands)
	filtered, unfiltered := terminalEventStatusEntries(events)
	if len(filtered) != 1 || len(unfiltered) != 1 {
		t.Fatalf("pre-release status entries filtered=%d unfiltered=%d, want admission only and the held cleanup entry: %+v", len(filtered), len(unfiltered), events)
	}
	requireTerminalEventCommandCompleted(t, events, filtered[0])
	for _, event := range events {
		if event.CommandID == unfiltered[0].CommandID && event.Stage != "entry" {
			t.Fatalf("held cleanup snapshot progressed past entry before release: %+v", events)
		}
	}
}

type abandonedTerminalEventCaller struct {
	scopeErr     error
	admissionErr error
	expired      bool
	identityErr  error
	reconcileErr error
}

// TestTerminalEventClockBubbleJoinsCallersBeforeClientClose registers an
// admission caller on a running row and returns from the bubble body at once,
// as a Fatal or early error would. The caller waits for the bubble's scope
// cancellation before admission entry, so admission returns that cancellation
// from its first context check while the operation remains unexpired. The
// caller then waits until every other bubble goroutine is durably blocked,
// reads its operation journal and issues a native command. Both succeed only
// if the bubble cancelled and joined the caller before client Close and before
// the operation cleanup.
func TestTerminalEventClockBubbleJoinsCallersBeforeClientClose(t *testing.T) {
	supervisor := newTerminalEventClockSupervisor(t)
	runTerminalEventClockBubble(t, supervisor, func(t *testing.T, scope *terminalEventClockScope) {
		joined := make(chan abandonedTerminalEventCaller, 1)
		// Registered first, so it runs after client Close and after every
		// operation cleanup registered below.
		t.Cleanup(func() {
			select {
			case observed := <-joined:
				if !errors.Is(observed.scopeErr, context.Canceled) || !errors.Is(observed.admissionErr, context.Canceled) ||
					errors.Is(observed.admissionErr, inspection.ErrAdmissionExpired) || observed.expired ||
					observed.identityErr != nil || observed.reconcileErr != nil {
					t.Errorf("abandoned caller scope=%v admission=%v expired=%t identity=%v reconcile=%v, want scope cancellation before admission entry on an unexpired operation, then open journal and client",
						observed.scopeErr, observed.admissionErr, observed.expired, observed.identityErr, observed.reconcileErr)
				}
			default:
				t.Error("abandoned caller was not joined before client close and operation cleanup")
			}
		})
		probe := newTerminalEventClockInspection(t, supervisor, 0, true)
		admissionfixture.WriteTerminalEventStatus(t, supervisor.store, probe.job, terminalEventRunningState())
		scope.callers.Go(func() {
			<-scope.ctx.Done()
			scopeErr := scope.ctx.Err()
			_, admissionErr := awaitInspectionWithSubmissionState(scope.ctx, probe.operation, supervisor.client, inspectionSubmissionState{})
			expired := probe.operation.Expired(time.Now())
			synctest.Wait()
			ctx, cancel := context.WithTimeout(context.Background(), inspection.AdmissionTimeout)
			defer cancel()
			identity, identityErr := InspectionIdentityContext(ctx, probe.operation)
			_, reconcileErr := supervisor.client.ReconcileInspection(ctx, identity)
			joined <- abandonedTerminalEventCaller{
				scopeErr: scopeErr, admissionErr: admissionErr, expired: expired,
				identityErr: identityErr, reconcileErr: reconcileErr,
			}
		})
	})
}
