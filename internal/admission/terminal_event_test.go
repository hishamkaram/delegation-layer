package admission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAwaitInspectionAcceptsPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	admissionfixture.WriteTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if enabled, enabledErr := supervisor.PrivateTerminalEventsEnabled(ctx); enabledErr != nil || !enabled {
		t.Fatalf("terminal events enabled=%t err=%v", enabled, enabledErr)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
	if event, found, readErr := supervisor.ReadPrivateTerminalEvent(ctx, job.ID); readErr != nil || !found || !event.Succeeded {
		t.Fatalf("terminal event before await = %+v found=%t err=%v", event, found, readErr)
	}
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor, inspectionSubmissionState{})
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	admissionfixture.WaitForTestFileContent(t, removals, "7\n")
	assertTerminalMaintenanceStatusCalls(t, statusCalls.Load(), unfilteredStatusCalls.Load(), 3)
	admissionfixture.WaitForPrivateTerminalEventRetirement(t, ctx, supervisor, job.ID)
}

func TestConcurrentCompletedInspectionsUseExactQueueObservations(t *testing.T) {
	// Publish both wakes before running maintenance so exact batch counts do
	// not depend on the scheduler's bounded quiet window.
	supervisor := newTerminalEventClockSupervisor(t)
	runTerminalEventClockBubble(t, supervisor, func(t *testing.T, scope *terminalEventClockScope) {
		// Fixture journals are fsync-heavy and created serially before observation.
		// Two operations prove concurrent exact reconciliation deterministically;
		// the native private-CLI acceptance proves 200-task production capacity.
		const count = 2
		operations := make([]*inspection.Operation, 0, count)
		jobs := make([]pueue.Job, 0, count)
		ids := make([]int64, 0, count)
		for index := range count {
			probe := newTerminalEventClockInspection(t, supervisor, index, true)
			operations, jobs, ids = append(operations, probe.operation), append(jobs, probe.job), append(ids, probe.job.ID)
		}
		writeTerminalEventStatuses(t, supervisor.store, jobs)
		baseline := requireTerminalEventClockBaseline(t, supervisor, ids...)

		ctx, cancel := context.WithTimeout(scope.ctx, inspection.AdmissionTimeout)
		defer cancel()
		type result struct {
			taskID string
			facts  []byte
			err    error
		}
		results := make(chan result, count)
		for _, operation := range operations {
			scope.callers.Go(func() {
				facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor.client, inspectionSubmissionState{})
				results <- result{taskID: operation.Request().TaskID, facts: facts, err: err}
			})
		}
		for range count {
			select {
			case observed := <-results:
				if observed.err != nil || string(observed.facts) != "{\"eligible\":true}\n" {
					t.Fatalf("exact admission task=%s facts=%s err=%v status_calls=%d unfiltered_status_calls=%d", observed.taskID, observed.facts, observed.err, supervisor.statusCalls.Load(), supervisor.unfilteredStatusCalls.Load())
				}
			case <-ctx.Done():
				t.Fatalf("coalesced admissions did not finish: %v", ctx.Err())
			}
		}
		for _, operation := range operations {
			requireTerminalEventClockUnexpired(t, operation)
		}
		requireTerminalEventClockCandidates(t, ctx, supervisor, baseline, ids...)
		runInspectionMaintenanceBatch(t, ctx, supervisor.client, supervisor.launches.Load(), count)
		removed, err := os.ReadFile(filepath.Join(supervisor.store.Root, ".supervisor", "terminal-event-removals"))
		if err != nil {
			t.Fatal(err)
		}
		removedIDs := strings.Fields(string(removed))
		slices.Sort(removedIDs)
		if !slices.Equal(removedIDs, []string{"7", "8"}) {
			t.Fatalf("concurrent exact cleanup removals=%v, want [7 8]", removedIDs)
		}
		// One admission observation and one candidate revalidation per
		// inspection, plus one streamed cleanup authorization/revalidation pair.
		requireTerminalEventClockCleanup(t, ctx, supervisor, baseline, count*2, 2, ids...)
	})
}

func runInspectionMaintenanceBatch(t *testing.T, ctx context.Context, supervisor *pueue.Client, launches, want int32) {
	t.Helper()
	if launches != want {
		t.Fatalf("maintenance launch requests=%d, want %d", launches, want)
	}
	if !supervisor.RunRequestedPrivateQueueMaintenance(ctx) {
		t.Fatal("persisted maintenance did not start")
	}
	admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, ctx, supervisor)
}
