package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAwaitInspectionDoesNotWaitForBlockedPrivateCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	blockPath := filepath.Join(store.Root, ".supervisor", "block-cleanup-remove")
	releasePath := filepath.Join(store.Root, ".supervisor", "release-cleanup-remove")
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(
		t,
		store,
		"FAKE_BLOCK_CLEANUP_REMOVE="+blockPath,
		"FAKE_RELEASE_CLEANUP_REMOVE="+releasePath,
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
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- runTerminalEvent(runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"}, terminalEventMaintenanceDependencies(
			statusCalls,
			unfilteredStatusCalls,
			"FAKE_BLOCK_CLEANUP_REMOVE="+blockPath,
			"FAKE_RELEASE_CLEANUP_REMOVE="+releasePath,
		))
	}()
	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-cleanup-blocked"), 15*time.Second)
	type result struct {
		facts []byte
		err   error
	}
	results := make(chan result, 1)
	go func() {
		facts, err := admissioncore.AwaitInspection(ctx, operation, supervisor)
		results <- result{facts: facts, err: err}
	}()

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
	if callbackErr := <-callbackDone; callbackErr != nil {
		t.Fatal(callbackErr)
	}
	waitForTestPathWithin(t, filepath.Join(store.Root, ".supervisor", "terminal-event-removals"), 15*time.Second)
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 2 || unfiltered != 2 {
		t.Fatalf("blocked callback cleanup made status_calls=%d unfiltered_status_calls=%d, want one streamed authorization/revalidation pair and no admission-side cleanup observation", total, unfiltered)
	}
}

func TestTerminalInspectionWakeRetriesConcurrentFailedCleanupWithoutAnotherCallback(t *testing.T) {
	store := newBareInspectionStore(t)
	base := filepath.Join(store.Root, ".supervisor")
	blockPath := filepath.Join(base, "block-cleanup-status")
	releasePath := filepath.Join(base, "release-cleanup-status")
	failOnce := filepath.Join(base, "fail-cleanup-remove-once")
	environment := []string{
		"FAKE_BLOCK_CLEANUP_STATUS=" + blockPath,
		"FAKE_RELEASE_CLEANUP_STATUS=" + releasePath,
		"FAKE_FAIL_CLEANUP_REMOVE_ONCE=" + failOnce,
	}
	supervisor, statusCalls, unfilteredStatusCalls := newTerminalEventInspectionSupervisorWithEnvironment(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, path := range []string{blockPath, failOnce} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	released := false
	defer func() {
		if !released {
			if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- runTerminalEvent(
			runnerArguments{Root: store.Root, PueueID: job.ID, ExitCode: "0"},
			terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls, environment...),
		)
	}()
	waitForTestPathWithin(t, filepath.Join(base, "terminal-event-status-blocked"), 15*time.Second)
	facts, err := admissioncore.AwaitInspection(ctx, operation, supervisor)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("terminal inspection facts=%s err=%v", facts, err)
	}
	if err = os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released = true
	if callbackErr := <-callbackDone; callbackErr != nil {
		t.Fatal(callbackErr)
	}
	waitForTestFileContent(t, filepath.Join(base, "terminal-event-removals"), "7\n")
	waitForPrivateQueueMaintenanceIdle(t, t.Context(), supervisor)
	if _, err = os.Lstat(failOnce); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first failed removal was not consumed: %v", err)
	}
	if total, unfiltered := statusCalls.Load(), unfilteredStatusCalls.Load(); total != 5 || unfiltered != 4 {
		t.Fatalf("failed-pass retry made status_calls=%d unfiltered_status_calls=%d, want admission reconciliation and two streamed authorization/revalidation pairs", total, unfiltered)
	}
}

func seedBlockedTerminalInspectionEvent(t *testing.T, ctx context.Context, store *taskdir.Store, blockPath string, supervisor *pueue.Client, operation *inspection.Operation, job pueue.Job) {
	t.Helper()
	if err := os.WriteFile(blockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordReceiptContext(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	writeTerminalEventStatus(t, store, job, admissionfixture.TerminalEventDoneState(job.Succeeded))
	if err := supervisor.RegisterPrivateTerminalEvent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := pueue.RecordPrivateTerminalEvent(ctx, store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
}

func runTerminalEventMaintenanceForTest(t *testing.T, store *taskdir.Store, id int64, statusCalls, unfilteredStatusCalls *atomic.Int32, environment ...string) {
	t.Helper()
	err := runTerminalEvent(
		runnerArguments{Root: store.Root, PueueID: id, ExitCode: "0"},
		terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls, environment...),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func terminalEventMaintenanceDependencies(statusCalls, unfilteredStatusCalls *atomic.Int32, environment ...string) Dependencies {
	options := pueue.Options{
		ObservationTimeout: inspection.AdmissionTimeout,
		Environment:        append(os.Environ(), environment...),
	}
	if statusCalls != nil {
		options.Observer = func(event pueue.CommandEvent) {
			if event.Stage != "entry" || !slices.Contains(event.Argv, "status") {
				return
			}
			statusCalls.Add(1)
			if unfilteredStatusCalls != nil && !slices.ContainsFunc(event.Argv, admissionfixture.TerminalMaintenanceLabelFilter) {
				unfilteredStatusCalls.Add(1)
			}
		}
	}
	return Dependencies{SupervisorOptions: options}.normalized()
}

func waitForTestFileContent(t *testing.T, path, want string) {
	t.Helper()
	admissionfixture.WaitForTestFileContent(t, path, want)
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
	if err = admissioncore.SubmitInspectionOnce(ctx, operation, supervisor, runner, store.Root); err != nil {
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
	t.Helper()
	return admissionfixture.NewTerminalEventInspectionSupervisor(t, store, admissioncore.TerminalEvidence{})
}

func newTerminalEventInspectionSupervisorWithEnvironment(t *testing.T, store *taskdir.Store, extraEnvironment ...string) (*pueue.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return admissionfixture.NewTerminalEventInspectionSupervisorWithEnvironment(t, store, admissioncore.TerminalEvidence{}, extraEnvironment...)
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
	recorded, recordErr := supervisorcore.OrdinarySchedulingRecorded(store.Root, supervisor.Binding())
	if recordErr != nil || !recorded {
		t.Fatalf("private scheduling unrecorded: recorded=%t err=%v binding=%+v runtime=%+v", recorded, recordErr, supervisor.Binding(), supervisor.RuntimeBinding())
	}
	if _, gateErr := supervisorcore.SubmissionGate(store.Root)(ctx, supervisor.Binding()); gateErr != nil {
		t.Fatalf("submission gate: %v", gateErr)
	}
}

func writeTerminalEventStatus(t *testing.T, store *taskdir.Store, job pueue.Job, state map[string]any) {
	t.Helper()
	admissionfixture.WriteTerminalEventStatus(t, store, job, state)
}
