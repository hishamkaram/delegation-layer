package pueue

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// Race-instrumented repository runs execute several process-heavy packages in
// parallel. Keep fixture bootstrap load tolerance separate from the 10-second
// stop protocol deadline exercised below.
const stopIdentityLeaseFixtureSetupTimeout = 30 * time.Second

func TestPrivateStopExcludesMutationsUntilPendingCompletion(t *testing.T) {
	for _, testCase := range []stopIdentityLeaseTestCase{
		{name: "task queued", state: StateQueued, action: "remove"},
		{name: "task running", state: StateRunning, action: "kill"},
		{name: "inspection queued", state: StateQueued, action: "remove", inspection: true},
		{name: "inspection running", state: StateRunning, action: "kill", inspection: true},
	} {
		t.Run(testCase.name, func(t *testing.T) { runStopIdentityLeaseTest(t, testCase) })
	}
}

type stopIdentityLeaseTestCase struct {
	name       string
	state      State
	action     string
	inspection bool
}

func runStopIdentityLeaseTest(t *testing.T, testCase stopIdentityLeaseTestCase) {
	t.Helper()
	fixture := newStopIdentityLeaseTestFixture(t, testCase)
	base := filepath.Dir(fixture.supervisor.configPath)
	status := newStopIdentityLeaseTestGate(t, base, "stop-status")
	mutation := newStopIdentityLeaseTestGate(t, base, "stop-mutation")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stopContext, cancelStop := context.WithCancel(ctx)
	call := startStopIdentityLeaseTestCall(t, stopContext, cancelStop, fixture.stop, status, mutation)

	// The fixture has emitted the exact matching row, but has not exited its
	// status command. Neither an add nor another remover may pass this scope.
	status.wait(t, ctx, call.done)
	assertStopIdentityLeaseTestExcluded(t, fixture)
	status.release()
	mutation.wait(t, ctx, call.done)

	// Detach only the observer while the actual remove/kill is still blocked.
	// Returning Pending must transfer, rather than release, the exclusive lock.
	cancelStop()
	select {
	case <-call.done:
	case <-ctx.Done():
		t.Fatalf("public stop did not detach from its pending mutation: %v", ctx.Err())
	}
	assertStopIdentityLeaseTestPending(t, testCase, call)
	select {
	case <-call.result.Pending.Done():
		t.Fatal("stop mutation completed before its fixture gate was released")
	default:
	}
	assertStopIdentityLeaseTestExcluded(t, fixture)
	mutation.release()
	settled := waitPending(t, call.result.Pending)
	if settled.Err != nil || !settled.Started || settled.ExitCode != 0 {
		t.Fatalf("stop mutation did not exit naturally: %+v", settled)
	}
	assertStopIdentityLeaseTestReleased(t, fixture, testCase.action)
}

type stopIdentityLeaseTestFixture struct {
	supervisor      *fakeSupervisor
	stop            func(context.Context) (StopResult, error)
	submission      *taskdir.SubmissionPermit
	launch          Launch
	cleanupIdentity Identity
}

func newStopIdentityLeaseTestFixture(t *testing.T, testCase stopIdentityLeaseTestCase) stopIdentityLeaseTestFixture {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	writeExecutable(t, private.clientPath, stopIdentityLeaseTestScript+privateClientFixture)
	ctx, cancel := context.WithTimeout(t.Context(), stopIdentityLeaseFixtureSetupTimeout)
	defer cancel()
	client, err := BindPrivate(ctx, private.clientPath, private.daemonPath, private.stateRoot, Options{ObservationTimeout: stopIdentityLeaseFixtureSetupTimeout})
	supervisorLeaseTestSettleClient(t, client, err)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(client.Binding().ConfigPath)
	fake := &fakeSupervisor{
		client: client, executable: private.clientPath, configPath: client.Binding().ConfigPath,
		statusPath: filepath.Join(base, "test-status.json"), logPath: filepath.Join(base, "stop-mutations.log"),
	}
	if writeErr := os.WriteFile(fake.logPath, nil, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	target := newTaskFixture(t, client.Binding())
	record := createSubmission(t, target, client.Binding())
	identity := identityFromRecord(record)
	targetID := int64(7)
	identity.NumericTaskID = &targetID
	cleanupID := int64(8)
	cleanupTask := "0" + record.TaskID[1:]
	if record.TaskID[0] == '0' {
		cleanupTask = "1" + record.TaskID[1:]
	}
	cleanupIdentity := Identity{
		RootID: record.RootID, TaskID: cleanupTask, NumericTaskID: &cleanupID,
		SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
	}
	jobs, groups := stopIdentityLeaseTestRows(testCase, identity, cleanupIdentity)
	if writeErr := os.WriteFile(fake.statusPath, statusTestPayload(t, jobs, groups), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	stopper, err := client.WithObservationTimeout(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Only the stop caller enters the FIFO faults. Competitor commands remain
	// finite even when a broken lock admits them and an assertion fails.
	stopper.options.Environment = append(environmentForCommand(stopper.options.Environment), "STOP_IDENTITY_LEASE_FAULT=1")
	if !testCase.inspection {
		recordStopIdentityLeaseTestReceipt(t, ctx, fake, target, identity)
	}
	fixture := stopIdentityLeaseTestFixture{
		supervisor: fake, cleanupIdentity: cleanupIdentity,
		stop: prepareStopIdentityLeaseTestCall(t, stopper, target, identity, testCase.inspection),
	}
	prepareStopIdentityLeaseTestSubmission(t, &fixture, jobs, groups)
	return fixture
}

func stopIdentityLeaseTestRows(testCase stopIdentityLeaseTestCase, identity, cleanup Identity) (map[string]any, map[string]any) {
	state := map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}
	if testCase.state == StateRunning {
		state = map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
	}
	job := statusTestJob(7, identity.Label(), state)
	groups := map[string]any{}
	if testCase.inspection {
		inspection := InspectionIdentity{RootID: identity.RootID, TaskID: identity.TaskID}
		job["label"], job["group"] = inspection.Label(), inspection.Group()
		groups[inspection.Group()] = map[string]any{"status": "Running", "parallel_tasks": 1}
	}
	ended := map[string]any{"Done": map[string]any{
		"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success",
	}}
	return map[string]any{"7": job, "8": statusTestJob(8, cleanup.Label(), ended)}, groups
}

func prepareStopIdentityLeaseTestCall(t *testing.T, stopper *Client, target *taskFixture, identity Identity, inspection bool) func(context.Context) (StopResult, error) {
	t.Helper()
	if inspection {
		permit := &inspectionTestPermit{
			stopRoot: identity.RootID, stopTask: identity.TaskID,
			stopTarget: identity.NumericTaskID, stopSupervisor: stopper.Binding(),
		}
		return func(callContext context.Context) (StopResult, error) {
			return stopper.StopInspection(callContext, permit, InspectionIdentity{
				RootID: identity.RootID, TaskID: identity.TaskID, NumericTaskID: identity.NumericTaskID,
			})
		}
	}
	permit := createStopPermit(t, target)
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	return func(callContext context.Context) (StopResult, error) { return stopper.Stop(callContext, permit) }
}

func recordStopIdentityLeaseTestReceipt(t *testing.T, ctx context.Context, fake *fakeSupervisor, target *taskFixture, identity Identity) {
	t.Helper()
	observation, err := fake.client.Reconcile(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if recordErr := target.taskDir.RecordSupervisorReceiptContext(ctx, *receipt); recordErr != nil {
		t.Fatal(recordErr)
	}
}

func prepareStopIdentityLeaseTestSubmission(t *testing.T, fixture *stopIdentityLeaseTestFixture, jobs, groups map[string]any) {
	t.Helper()
	target := newTaskFixture(t, fixture.supervisor.client.Binding())
	permit, err := target.taskDir.PrepareSubmission(fixture.supervisor.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	record, err := permit.Record()
	if err != nil {
		t.Fatal(err)
	}
	jobs["9"] = statusTestJob(9, record.Label, map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	path := filepath.Join(filepath.Dir(fixture.supervisor.configPath), "stop-after-add.json")
	if writeErr := os.WriteFile(path, statusTestPayload(t, jobs, groups), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	fixture.submission = permit
	fixture.launch = Launch{RunnerExecutable: fixture.supervisor.executable, RootPath: target.store.Root}
}

type stopIdentityLeaseTestCall struct {
	done   chan struct{}
	result StopResult
	err    error
}

func startStopIdentityLeaseTestCall(t *testing.T, ctx context.Context, cancel context.CancelFunc, stop func(context.Context) (StopResult, error), status, mutation *stopIdentityLeaseTestGate) *stopIdentityLeaseTestCall {
	t.Helper()
	call := &stopIdentityLeaseTestCall{done: make(chan struct{})}
	t.Cleanup(func() {
		status.release()
		mutation.release()
		cancel()
		<-call.done
		for _, pending := range pendingHandles(call.err) {
			waitPending(t, pending)
		}
	})
	go func() {
		call.result, call.err = stop(ctx)
		close(call.done)
	}()
	return call
}

func assertStopIdentityLeaseTestPending(t *testing.T, testCase stopIdentityLeaseTestCase, call *stopIdentityLeaseTestCall) {
	t.Helper()
	result := call.result
	if !errors.Is(call.err, ErrInFlight) || result.Pending == nil || !result.InFlight || result.Acknowledged != nil || pendingFrom(call.err) != result.Pending {
		t.Fatalf("public stop lost its unfinished mutation: result=%+v err=%v", result, call.err)
	}
	if !result.Requested || !result.Matched || !result.Attempted || result.ObservedState != testCase.state || result.Action != testCase.action {
		t.Fatalf("public stop did not route the matched row exactly: %+v", result)
	}
	if result.NumericTaskID == nil || *result.NumericTaskID != 7 {
		t.Fatalf("public stop changed its saved target: %+v", result)
	}
}

func assertStopIdentityLeaseTestExcluded(t *testing.T, fixture stopIdentityLeaseTestFixture) {
	t.Helper()
	submitContext, cancelSubmit := context.WithTimeout(t.Context(), 50*time.Millisecond)
	observation, submitErr := fixture.supervisor.client.Submit(submitContext, fixture.submission, fixture.launch)
	cancelSubmit()
	for _, pending := range pendingHandles(submitErr) {
		waitPending(t, pending)
	}
	if !errors.Is(submitErr, context.DeadlineExceeded) || observation.Matched || observation.Pending != nil {
		t.Fatalf("submission entered the public stop's exclusive scope: observation=%+v err=%v", observation, submitErr)
	}
	cleanupContext, cancelCleanup := context.WithTimeout(t.Context(), 50*time.Millisecond)
	cleanupErr := fixture.supervisor.client.RemoveCompletedTask(cleanupContext, fixture.cleanupIdentity)
	cancelCleanup()
	for _, pending := range pendingHandles(cleanupErr) {
		waitPending(t, pending)
	}
	if !errors.Is(cleanupErr, context.DeadlineExceeded) {
		t.Fatalf("completed-row cleanup entered the public stop's exclusive scope: %v", cleanupErr)
	}
	invocations := readInvocations(t, fixture.supervisor.logPath)
	if countCommand(invocations, "add") != 0 || stopIdentityLeaseTestCommandCount(invocations, "remove", "8") != 0 {
		t.Fatalf("a competing mutation started before the stop settled: %v", invocations)
	}
}

func assertStopIdentityLeaseTestReleased(t *testing.T, fixture stopIdentityLeaseTestFixture, action string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	observation, err := fixture.supervisor.client.Submit(ctx, fixture.submission, fixture.launch)
	for _, pending := range pendingHandles(err) {
		waitPending(t, pending)
	}
	if err != nil || !observation.Matched || observation.NumericTaskID != 9 {
		t.Fatalf("submission permit was consumed or lock retained after stop completion: observation=%+v err=%v", observation, err)
	}
	if cleanupErr := fixture.supervisor.client.RemoveCompletedTask(ctx, fixture.cleanupIdentity); cleanupErr != nil {
		for _, pending := range pendingHandles(cleanupErr) {
			waitPending(t, pending)
		}
		t.Fatalf("cleanup could not enter after stop completion: %v", cleanupErr)
	}
	invocations := readInvocations(t, fixture.supervisor.logPath)
	if countCommand(invocations, "add") != 1 || stopIdentityLeaseTestCommandCount(invocations, action, "7") != 1 || stopIdentityLeaseTestCommandCount(invocations, "remove", "8") != 1 {
		t.Fatalf("stop or competing mutations were lost or repeated: %v", invocations)
	}
}

func stopIdentityLeaseTestCommandCount(invocations [][]string, action, target string) int {
	count := 0
	for _, args := range invocations {
		if len(args) == 4 && args[2] == action && args[3] == target {
			count++
		}
	}
	return count
}

type stopIdentityLeaseTestGate struct {
	ready   chan struct{}
	release func()
	err     error
}

func newStopIdentityLeaseTestGate(t *testing.T, base, name string) *stopIdentityLeaseTestGate {
	t.Helper()
	seen := openStopIdentityLeaseTestFIFO(t, filepath.Join(base, name+"-seen"))
	release := openStopIdentityLeaseTestFIFO(t, filepath.Join(base, name+"-release"))
	gate := &stopIdentityLeaseTestGate{ready: make(chan struct{})}
	gate.release = sync.OnceFunc(func() {
		if _, writeErr := release.WriteString("release\n"); writeErr != nil {
			t.Error(writeErr)
		}
	})
	go func() {
		var signal [1]byte
		_, gate.err = io.ReadFull(seen, signal[:])
		close(gate.ready)
	}()
	t.Cleanup(func() {
		gate.release()
		// Wake the local reader even if a regression prevented command start.
		// The command owner is joined before these FIFO cleanup callbacks run.
		if _, writeErr := seen.WriteString("x"); writeErr != nil {
			t.Error(writeErr)
		}
		<-gate.ready
	})
	return gate
}

func openStopIdentityLeaseTestFIFO(t *testing.T, path string) *os.File {
	t.Helper()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return file
}

func (gate *stopIdentityLeaseTestGate) wait(t *testing.T, ctx context.Context, callDone <-chan struct{}) {
	t.Helper()
	select {
	case <-gate.ready:
		if gate.err != nil {
			t.Fatalf("fixture checkpoint failed: %v", gate.err)
		}
	case <-callDone:
		t.Fatal("public stop returned before its fixture checkpoint")
	case <-ctx.Done():
		t.Fatalf("public stop did not reach its fixture checkpoint: %v", ctx.Err())
	}
}

const stopIdentityLeaseTestScript = `#!/bin/sh
set -eu
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4'; exit 0 ;;
  status)
    if [ "${STOP_IDENTITY_LEASE_FAULT:-}" = 1 ]; then
      base=${2%/*}
      cat "$base/test-status.json"
      printf x > "$base/stop-status-seen"
      IFS= read -r release < "$base/stop-status-release"
      exit 0
    fi
    ;;
  add|remove|kill)
    base=${2%/*}
    {
      printf 'argc=%s\n' "$#"
      for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
      printf '%s\n' '---'
    } >> "$base/stop-mutations.log"
    if [ "$3" = add ]; then
      cp "$base/stop-after-add.json" "$base/test-status.json"
      printf '%s\n' 9
    elif [ "${STOP_IDENTITY_LEASE_FAULT:-}" = 1 ]; then
      [ "${4-}" = 7 ] || exit 64
      printf x > "$base/stop-mutation-seen"
      IFS= read -r release < "$base/stop-mutation-release"
    fi
    exit 0
    ;;
esac
`
