package admission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAwaitInspectionRejectsFailedPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job.Succeeded = false
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
		t.Fatalf("successful callback hint for failed row = %+v found=%t err=%v", event, found, readErr)
	}
	facts, err := AwaitInspection(ctx, operation, supervisor)
	if facts != nil || !errors.Is(err, ErrInspectionWorkerUnavailable) {
		t.Fatalf("failed terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	admissionfixture.WaitForTestPath(t, removals)
	admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	assertTerminalMaintenanceStatusCalls(t, statusCalls.Load(), unfilteredStatusCalls.Load(), 3)
	if _, succeeded, readErr := operation.ReadWorkerResultContext(ctx); readErr != nil || succeeded {
		t.Fatalf("failed terminal event did not persist failed worker observation: succeeded=%t err=%v", succeeded, readErr)
	}
}

func assertTerminalMaintenanceStatusCalls(t *testing.T, statusCalls, unfilteredStatusCalls, wantTotal int32) {
	t.Helper()
	if statusCalls != wantTotal || unfilteredStatusCalls != wantTotal-1 {
		t.Fatalf("completed inspection made status_calls=%d unfiltered_status_calls=%d, want %d total with one streamed cleanup authorization/revalidation pair", statusCalls, unfilteredStatusCalls, wantTotal)
	}
}

func TestAwaitInspectionFallsBackToExactRowWhenTerminalEventRegistrationOrCallbackFails(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		terminalEventRegistered        bool
		terminalEventRegistrationKnown bool
	}{{
		name:                           "registration failure",
		terminalEventRegistrationKnown: true,
	}, {
		name:                           "callback failure",
		terminalEventRegistrationKnown: true,
		terminalEventRegistered:        true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			testInspectionTerminalEventFallback(t, tc.terminalEventRegistrationKnown, tc.terminalEventRegistered)
		})
	}
}

func testInspectionTerminalEventFallback(t *testing.T, registrationKnown, registered bool) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	admissionfixture.WriteTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(job.Succeeded))
	if registered {
		if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor, inspectionSubmissionState{
		terminalEventRegistrationKnown: registrationKnown,
		terminalEventRegistered:        registered,
	})
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("exact terminal-row fallback facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	admissionfixture.WaitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	removed, readErr := os.ReadFile(removals)
	if readErr != nil {
		t.Fatalf("private queue maintenance completed without removal record: %v", readErr)
	}
	if string(removed) != "7\n" {
		t.Fatalf("fallback removal contents=%q, want %q", removed, "7\n")
	}
	if got := statusCalls.Load(); got != 4 {
		t.Fatalf("fallback made %d status calls, want one exact reconciliation, one candidate revalidation and one cleanup authorization/revalidation pair", got)
	}
}

func TestDelayedTerminalEventAfterNumericIDReuseCannotAuthorizeNewInspection(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	admissionfixture.RequireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	admissionfixture.RequireAppTest(t, supervisor.ClearPrivateTerminalEvents(ctx, job.ID))
	admissionfixture.RequireAppTest(t, operation.RecordReceiptContext(ctx, job.ID))
	admissionfixture.WriteTerminalEventStatus(t, store, job, terminalEventRunningState())
	admissionfixture.RequireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	admissionfixture.RequireAppTest(t, pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"))
	replacement := job
	replacementLabel := "replacement-terminal-row"
	replacement.Label = &replacementLabel
	replacement.State = pueue.StateEnded
	replacement.Succeeded = true
	admissionfixture.WriteTerminalEventStatus(t, store, replacement, admissionfixture.TerminalEventDoneState(replacement.Succeeded))

	if event, found, readErr := supervisor.ReadPrivateTerminalEvent(ctx, job.ID); readErr != nil || !found || !event.Succeeded {
		t.Fatalf("delayed callback hint = %+v found=%t err=%v", event, found, readErr)
	}
	state := inspectionAwaitState{
		identity:                       operation.Request().SupervisorIdentity(),
		polls:                          inspectionPollSchedule{callbackExpected: true},
		terminalEvents:                 true,
		terminalEventRegistrationKnown: true,
		terminalEventRegistered:        true,
	}
	state.identity.NumericTaskID = &job.ID
	for attempt := range 2 {
		facts, observed, err := state.observe(ctx, operation, supervisor)
		if err != nil || observed || facts != nil {
			t.Fatalf("delayed predecessor callback attempt=%d authorized replacement: facts=%s observed=%t err=%v", attempt, facts, observed, err)
		}
	}
	if got := statusCalls.Load(); got != 1 {
		t.Fatalf("latched delayed callback made %d exact observations, want 1", got)
	}
	if _, _, readErr := operation.ReadWorkerResultContext(ctx); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("delayed predecessor callback published worker authority: %v", readErr)
	}
	if facts, recorded, terminal, readErr := RecordedInspectionFacts(ctx, operation); facts != nil || recorded || terminal || readErr != nil {
		t.Fatalf("delayed predecessor callback granted admission authority: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, readErr)
	}
}

func writeTerminalEventStatuses(t *testing.T, store *taskdir.Store, jobs []pueue.Job) {
	t.Helper()
	tasks := make(map[string]any, len(jobs))
	groups := make(map[string]any)
	for _, job := range jobs {
		if job.Label == nil {
			t.Fatal("terminal-event status requires a job label")
		}
		parallelTasks := uint64(1)
		if job.Group == "default" {
			parallelTasks = 0
		}
		tasks[strconv.FormatInt(job.ID, 10)] = map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": admissionfixture.TerminalEventDoneState(job.Succeeded),
		}
		groups[job.Group] = map[string]any{"status": "Running", "parallel_tasks": parallelTasks}
	}
	status, err := json.Marshal(map[string]any{"tasks": tasks, "groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "terminal-event-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
}

func terminalEventRunningState() map[string]any {
	return map[string]any{"Running": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z",
	}}
}
