package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAwaitInspectionAcceptsPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, fullStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
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
	facts, err := awaitInspection(ctx, operation, supervisor)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	waitForTestFileContent(t, removals, "7\n")
	assertBoundedInspectionStatusCalls(t, statusCalls.Load(), fullStatusCalls.Load())
	waitForPrivateTerminalEventRetirement(t, ctx, supervisor, job.ID)
}

func waitForPrivateTerminalEventRetirement(t *testing.T, ctx context.Context, supervisor *pueue.Client, id int64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, found, err := supervisor.ReadPrivateTerminalEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("accepted terminal event was not retired: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestConcurrentCompletedInspectionsUseExactQueueObservations(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, fullStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	// Fixture journals are fsync-heavy and created serially before observation.
	// Two operations prove concurrent exact reconciliation deterministically;
	// the native private-CLI acceptance proves 200-task production capacity.
	const count = 2
	operations := make([]*inspection.Operation, 0, count)
	jobs := make([]pueue.Job, 0, count)
	for index := range count {
		taskID := fmt.Sprintf("%032x", index+1)
		operation, job := newTerminalInspectionCleanupOperationForTask(
			t, store, supervisor, inspection.ResultEligible, true, taskID, int64(index+7),
		)
		completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
		if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
		jobs = append(jobs, job)
	}
	writeTerminalEventStatuses(t, store, jobs)

	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	type result struct {
		taskID string
		facts  []byte
		err    error
	}
	results := make(chan result, count)
	for _, operation := range operations {
		go func() {
			facts, err := awaitInspection(ctx, operation, supervisor)
			results <- result{taskID: operation.Request().TaskID, facts: facts, err: err}
		}()
	}
	for range count {
		select {
		case observed := <-results:
			if observed.err != nil || string(observed.facts) != "{\"eligible\":true}\n" {
				t.Fatalf("exact admission task=%s facts=%s err=%v status_calls=%d full_status_calls=%d", observed.taskID, observed.facts, observed.err, statusCalls.Load(), fullStatusCalls.Load())
			}
		case <-ctx.Done():
			t.Fatalf("coalesced admissions did not finish: %v", ctx.Err())
		}
	}
	waitForTestPath(t, filepath.Join(store.Root, ".supervisor", "terminal-event-removals"))
	if total, full := statusCalls.Load(), fullStatusCalls.Load(); total-full != count || full < 1 || full > count {
		t.Fatalf("%d completed inspections made status_calls=%d full_status_calls=%d, want one exact observation per inspection plus bounded cleanup", count, total, full)
	}
}

func TestAwaitInspectionDoesNotWaitForBlockedPrivateCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	blockPath := filepath.Join(store.Root, ".supervisor", "block-full-status")
	releasePath := filepath.Join(store.Root, ".supervisor", "release-full-status")
	supervisor, statusCalls, fullStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(
		t,
		store,
		"FAKE_BLOCK_FULL_STATUS="+blockPath,
		"FAKE_RELEASE_FULL_STATUS="+releasePath,
	)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	released := false
	defer func() {
		if !released {
			if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}()

	seedBlockedTerminalInspectionEvent(t, ctx, store, blockPath, supervisor, operation, job)
	type result struct {
		facts []byte
		err   error
	}
	results := make(chan result, 1)
	go func() {
		facts, err := awaitInspection(ctx, operation, supervisor)
		results <- result{facts: facts, err: err}
	}()

	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-maintenance"), 15*time.Second)
	if got := statusCalls.Load() - fullStatusCalls.Load(); got != 1 {
		t.Fatalf("blocked cleanup path made %d exact status calls, want one", got)
	}
	admissionTimer := time.NewTimer(15 * time.Second)
	defer admissionTimer.Stop()
	select {
	case observed := <-results:
		if observed.err != nil || string(observed.facts) != "{\"eligible\":true}\n" {
			t.Fatalf("terminal event admission facts=%s err=%v", observed.facts, observed.err)
		}
	case <-admissionTimer.C:
		t.Fatal("admission waited for root-wide cleanup")
	case <-ctx.Done():
		t.Fatalf("admission context ended before blocked cleanup returned: %v", ctx.Err())
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released = true
	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-removals"), 15*time.Second)
}

func seedBlockedTerminalInspectionEvent(t *testing.T, ctx context.Context, store *taskdir.Store, blockPath string, supervisor *pueue.Client, operation *inspection.Operation, job pueue.Job) {
	t.Helper()
	if err := os.WriteFile(blockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitInspectionRejectsFailedPrivateTerminalEventAfterCurrentQueueReconciliation(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls, fullStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job.Succeeded = false
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
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
	facts, err := awaitInspection(ctx, operation, supervisor)
	if facts != nil || !errors.Is(err, errInspectionWorkerUnavailable) {
		t.Fatalf("failed terminal event admission facts=%s err=%v", facts, err)
	}
	removals := filepath.Join(store.Root, ".supervisor", "terminal-event-removals")
	waitForTestPath(t, removals)
	assertBoundedInspectionStatusCalls(t, statusCalls.Load(), fullStatusCalls.Load())
	if _, succeeded, readErr := operation.ReadWorkerResultContext(ctx); readErr != nil || succeeded {
		t.Fatalf("failed terminal event did not persist failed worker observation: succeeded=%t err=%v", succeeded, readErr)
	}
}

func assertBoundedInspectionStatusCalls(t *testing.T, statusCalls, fullStatusCalls int32) {
	t.Helper()
	if statusCalls-fullStatusCalls != 1 || fullStatusCalls != 1 {
		t.Fatalf("completed inspection made status_calls=%d full_status_calls=%d, want one exact observation and one cleanup snapshot", statusCalls, fullStatusCalls)
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
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, terminalEventDoneState(job.Succeeded))
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
	waitForTestFileContent(t, removals, "7\n")
	if got := statusCalls.Load(); got != 2 {
		t.Fatalf("fallback made %d status calls, want one exact reconciliation and one maintenance snapshot", got)
	}
}

func waitForTestFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.NewTimer(pueue.DefaultObservationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if last == want {
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s content: got=%q want=%q", path, last, want)
		case <-ticker.C:
		}
	}
}

func TestDelayedTerminalEventAfterNumericIDReuseCannotAuthorizeNewInspection(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	requireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	requireAppTest(t, supervisor.ClearPrivateTerminalEvents(ctx, job.ID))
	requireAppTest(t, operation.RecordReceiptContext(ctx, job.ID))
	writeTerminalEventStatus(t, store, job, terminalEventRunningState())
	requireAppTest(t, supervisor.RegisterPrivateTerminalEvent(ctx, job.ID))
	requireAppTest(t, pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"))
	replacement := job
	replacementLabel := "replacement-terminal-row"
	replacement.Label = &replacementLabel
	replacement.State = pueue.StateEnded
	replacement.Succeeded = true
	writeTerminalEventStatus(t, store, replacement, terminalEventDoneState(replacement.Succeeded))

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
	if facts, recorded, terminal, readErr := recordedInspectionFacts(ctx, operation); facts != nil || recorded || terminal || readErr != nil {
		t.Fatalf("delayed predecessor callback granted admission authority: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, readErr)
	}
}

func TestSubmitInspectionOnceUsesConcurrentAddReceiptWithoutQueueSnapshot(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, statusCalls := newTerminalEventInspectionSupervisor(t, store)
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err = filepath.EvalSymlinks(runner)
	if err != nil {
		t.Fatal(err)
	}
	runnerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	request := newBareInspectionRequest(t, store)
	operation, err := inspection.OpenConcurrentOperation(store, request, inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   task.ComputeSHA256([]byte("runtime-definition")),
		CapabilitySHA256:   task.ComputeSHA256([]byte("runtime-capability")),
		HelperExecutable:   runner,
		HelperSHA256:       runnerSHA,
		WorkerExecutable:   runner,
		WorkerSHA256:       runnerSHA,
		Supervisor:         supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()

	assertTerminalEventInspectionGenerationReady(t, store, supervisor, ctx)
	if err = submitInspectionOnce(ctx, operation, supervisor, runner, store.Root); err != nil {
		t.Fatal(err)
	}
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 9 {
		t.Fatalf("concurrent inspection receipt = %+v err=%v", receipt, err)
	}
	if calls := statusCalls.Load(); calls != 0 {
		t.Fatalf("concurrent submission made %d queue-wide status calls", calls)
	}
}

func newTerminalEventInspectionSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, *atomic.Int32) {
	client, statusCalls, _ := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	return client, statusCalls
}

func newTerminalEventInspectionSupervisorWithEnvironment(t *testing.T, store *taskdir.Store, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	bundle := canonicalAppTestTempDir(t)
	clientPath := filepath.Join(bundle, "pueue")
	daemonPath := filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, clientPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
	    if [ "${5-}" = "" ]; then : > "$base/terminal-event-maintenance"; fi
	    if [ "${5-}" = "" ] && [ "${FAKE_BLOCK_FULL_STATUS:-}" != "" ] && [ -f "$FAKE_BLOCK_FULL_STATUS" ]; then
	      while [ ! -f "${FAKE_RELEASE_FULL_STATUS:?}" ]; do sleep 0.01; done
	    fi
    if [ -f "$base/terminal-event-status.json" ]; then
      cat "$base/terminal-event-status.json"
      exit 0
    fi
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":%s}}}\n' "$parallel"
    ;;
  group)
    parallel=1
    [ -f "$base/parallel-unlimited" ] && parallel=0
    printf '{"default":{"status":"Running","parallel_tasks":%s}}\n' "$parallel"
    ;;
  parallel) : > "$base/parallel-unlimited" ;;
  remove) printf '%s\n' "${4-}" >> "$base/terminal-event-removals" ;;
  add)
    if [ "${4-}" = --help ]; then printf '%s\n' '--stashed --print-task-id'; else printf '%s\n' '9'; fi
    ;;
  enqueue)
    if [ "${4-}" = --help ]; then printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'; fi
    ;;
  *) exit 64 ;;
esac
`)
	writeRunnerFixture(t, daemonPath, `#!/bin/sh
set -eu
[ "${1-}" = -c ]
base=${2%/*}
mkdir -p "$base/run"
printf '%s\n' "$$" > "$base/run/pueue.pid"
`)
	var statusCalls atomic.Int32
	var fullStatusCalls atomic.Int32
	options := pueue.Options{
		ObservationTimeout:          inspection.AdmissionTimeout,
		SuccessfulCleanupAuthorizer: authorizePrivateSuccessfulCleanup,
		Environment:                 append(os.Environ(), extraEnvironment...),
		Observer: func(event pueue.CommandEvent) {
			if event.Stage == "entry" && slices.Contains(event.Argv, "status") {
				statusCalls.Add(1)
				if !slices.ContainsFunc(event.Argv, func(argument string) bool { return strings.HasPrefix(argument, "label=") }) {
					fullStatusCalls.Add(1)
				}
			}
		},
	}
	bootstrap, err := pueue.BindPrivate(t.Context(), clientPath, daemonPath, store.Root, options)
	if err != nil {
		t.Fatal(err)
	}
	listenSupervisorProbeTestEndpoint(t, bootstrap.Binding().Endpoint)
	client, err := preparePrivateAdmissionSupervisor(Dependencies{
		InitialSupervisorExecutable: clientPath,
		SupervisorOptions:           options,
	}, store, options)
	settleSupervisorProbeTestClient(t, client, err)
	if closeErr := bootstrap.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	statusCalls.Store(0)
	fullStatusCalls.Store(0)
	for _, name := range []string{"terminal-event-maintenance", "terminal-event-removals"} {
		if removeErr := os.Remove(filepath.Join(store.Root, ".supervisor", name)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			t.Fatal(removeErr)
		}
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, &statusCalls, &fullStatusCalls
}

func assertTerminalEventInspectionGenerationReady(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, ctx context.Context) {
	t.Helper()
	complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, supervisor.Binding())
	if completeErr != nil || !complete {
		migration, migrationErr := pueue.PrivateSupervisorMigrationPending(store.Root)
		recovery, recoveryErr := pueue.PrivateRecoveryBindingPresent(store.Root)
		bootstrap, bootstrapErr := pueue.PrivateBootstrapPending(store.Root)
		daemon, daemonErr := pueue.PrivateDaemonIdentityPresent(store.Root)
		entries, readErr := os.ReadDir(filepath.Join(store.Root, ".supervisor"))
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("private generation incomplete: complete=%t err=%v migration=%t/%v recovery=%t/%v bootstrap=%t/%v daemon=%t/%v files=%v read=%v binding=%+v runtime=%+v", complete, completeErr, migration, migrationErr, recovery, recoveryErr, bootstrap, bootstrapErr, daemon, daemonErr, names, readErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	recorded, recordErr := privateOrdinarySchedulingRecorded(store.Root, supervisor.Binding())
	if recordErr != nil || !recorded {
		t.Fatalf("private scheduling unrecorded: recorded=%t err=%v binding=%+v runtime=%+v", recorded, recordErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	if gateErr := privateSubmissionGate(store.Root)(ctx, supervisor.Binding()); gateErr != nil {
		t.Fatalf("submission gate: %v", gateErr)
	}
}

func writeTerminalEventStatus(t *testing.T, store *taskdir.Store, job pueue.Job, state map[string]any) {
	t.Helper()
	if job.Label == nil {
		t.Fatal("terminal-event status requires a job label")
	}
	parallelTasks := uint64(1)
	if job.Group == "default" {
		parallelTasks = 0
	}
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(job.ID, 10): map[string]any{
			"id": job.ID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": *job.Label, "status": state,
		}},
		"groups": map[string]any{job.Group: map[string]any{"status": "Running", "parallel_tasks": parallelTasks}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "terminal-event-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
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
			"label": *job.Label, "status": terminalEventDoneState(job.Succeeded),
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

func terminalEventDoneState(succeeded bool) map[string]any {
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	return map[string]any{"Done": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z", "end": "2026-10-10T00:00:00Z", "result": result,
	}}
}

func terminalEventRunningState() map[string]any {
	return map[string]any{"Running": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z",
	}}
}
