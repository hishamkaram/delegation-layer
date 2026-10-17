package control

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestBindReconcileAndReceiptUseCompleteBinding(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	writeStatusFor(t, fake, rootID, taskID, 7, StateQueued)
	identity := Identity{RootID: rootID, TaskID: taskID, SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
	observation, err := fake.client.Reconcile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.State != StateQueued || observation.NumericTaskID != 7 {
		t.Fatalf("unexpected observation: %+v", observation)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.NumericTaskID != 7 || receipt.SupervisorRef() != fake.client.Binding() {
		t.Fatalf("receipt lost binding: %+v", receipt)
	}
	if err := task.ValidateSupervisorReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	var status []string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) > 2 && invocation[2] == "status" {
			status = invocation
		}
	}
	want := []string{"-c", fake.configPath, "status", "--json", "label=" + identity.Label()}
	if !slices.Equal(status, want) {
		t.Fatalf("wrong exact task status argv: got=%q want=%q", status, want)
	}
}

func TestReconcileDoesNotParseTheRetainedQueue(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	identity := Identity{RootID: rootID, TaskID: taskID, SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
	large, _ := statusStreamTestLargeQueue(t, 0)
	filtered := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, identity.Label(), statusStreamTestRunningState()),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if len(large) <= MaxControlBytes {
		t.Fatalf("retained queue fixture has only %d bytes", len(large))
	}
	filteredPath := filepath.Join(filepath.Dir(fake.statusPath), "filtered-status.json")
	if err := os.WriteFile(fake.statusPath, large, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filteredPath, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	fake.client.options.Environment = append(fake.client.options.Environment, "FAKE_FILTERED_STATUS="+filteredPath)

	observation, err := fake.client.Reconcile(context.Background(), identity)
	if err != nil || !observation.Matched || observation.NumericTaskID != 7 || observation.State != StateRunning {
		t.Fatalf("reconciliation did not use bounded exact status: observation=%+v err=%v", observation, err)
	}
}

func TestSnapshotIdentityCopiesMutableNumericTarget(t *testing.T) {
	target := int64(7)
	snapshot := snapshotIdentity(Identity{NumericTaskID: &target})
	target = 8
	if snapshot.NumericTaskID == nil || *snapshot.NumericTaskID != 7 {
		t.Fatalf("numeric target alias escaped snapshot: %+v", snapshot)
	}
}

func TestSubmitConsumesOnePermitAndUsesLiteralArguments(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root, Supervised: true})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.NumericTaskID != 7 || observation.State != StateQueued {
		t.Fatalf("submission was not reconciled: %+v", observation)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	invocations := readInvocations(t, fake.logPath)
	var add []string
	for _, args := range invocations {
		if len(args) > 2 && args[2] == "add" {
			add = args
		}
	}
	want := []string{"-c", fake.configPath, "add", "--escape", "--label", record.Label, "--print-task-id", "--", fake.executable, "--guardian", "--supervised", "--root", fixture.store.Root, record.TaskID}
	if !slices.Equal(add, want) {
		t.Fatalf("literal add argv changed: got=%q want=%q", add, want)
	}
	if countCommand(invocations, "add") != 1 {
		t.Fatalf("expected exactly one add: %+v", invocations)
	}
	if countCommand(invocations, "status") != 0 {
		t.Fatalf("successful add triggered a queue-wide status scan: %+v", invocations)
	}
}

func TestSubmitWithGateRejectsBeforePueueAdd(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	saved := fake.client.Binding()
	saved.ResolutionCwd = filepath.Join(t.TempDir(), "predecessor")
	fake.client.binding = saved
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	gateErr := errors.New("submission generation drift")
	gateCalls := 0
	observation, err := fake.client.SubmitWithGate(
		context.Background(),
		prepared,
		Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root},
		func(_ context.Context, binding task.SupervisorRef) (SubmissionAuthorization, error) {
			gateCalls++
			if binding != fake.client.RuntimeBinding() || binding == fake.client.Binding() {
				t.Fatalf("submission gate received durable binding instead of active runtime: got=%+v durable=%+v runtime=%+v", binding, fake.client.Binding(), fake.client.RuntimeBinding())
			}
			return SubmissionAuthorization{}, gateErr
		},
	)
	if !errors.Is(err, gateErr) || observation.Matched {
		t.Fatalf("gate refusal was not returned before admission: observation=%+v err=%v", observation, err)
	}
	if gateCalls != 1 {
		t.Fatalf("submission gate calls=%d, want one", gateCalls)
	}
	if prepared.IsConsumed() {
		t.Fatal("submission gate consumed the permit before pueue add")
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 0 {
		t.Fatal("gate refusal reached the pueue add command")
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitCustomRunnerOmitsSupervisedMarker(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root}); err != nil {
		t.Fatal(err)
	}
	if err = prepared.Release(); err != nil {
		t.Fatal(err)
	}
	var add []string
	for _, arguments := range readInvocations(t, fake.logPath) {
		if len(arguments) > 2 && arguments[2] == "add" {
			add = arguments
		}
	}
	want := []string{"-c", fake.configPath, "add", "--escape", "--label", record.Label, "--print-task-id", "--", fake.executable, "--root", fixture.store.Root, record.TaskID}
	if !slices.Equal(add, want) {
		t.Fatalf("custom runner add argv changed: got=%q want=%q", add, want)
	}
}

func TestSubmitNeverRetriesAfterUncertainAdmission(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	client, err := NewClient(fake.client.Binding(), Options{
		Environment: append(append([]string{}, fake.environment...), "FAKE_ADD_ID=not-a-task-id"),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root})
	if !errors.Is(err, ErrUnknown) || !errors.Is(err, ErrSubmissionUncertain) || observation.Matched {
		t.Fatalf("lost admission was treated as success: %+v %v", observation, err)
	}
	if _, err := client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root}); !errors.Is(err, task.ErrPermitAlreadyUsed) {
		t.Fatalf("second add did not stop at consumed permit: %v", err)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 1 {
		t.Fatal("uncertain admission retried add")
	}
}

func TestSubmitPreflightFailureIsNotSubmissionUncertain(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: "relative"})
	if err == nil || errors.Is(err, ErrSubmissionUncertain) || observation.Matched {
		t.Fatalf("preflight failure was reported as a possible submission: %+v %v", observation, err)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 0 {
		t.Fatal("invalid launch reached the pueue add command")
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitStartFailureIsNotSubmissionUncertain(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	injectedFailure := &submitStartFailure{
		executable: fake.executable,
		hidden:     filepath.Join(filepath.Dir(fake.executable), "pueue-unavailable"),
	}
	fake.client.options.Observer = injectedFailure.observe
	t.Cleanup(func() {
		if cleanupErr := injectedFailure.restore(); cleanupErr != nil {
			t.Errorf("restore fake supervisor after start failure: %v", cleanupErr)
		}
	})

	observation, err := fake.client.Submit(context.Background(), prepared, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root})
	if injectedFailure.err != nil {
		t.Fatalf("inject or restore supervisor start failure: %v", injectedFailure.err)
	}
	if !injectedFailure.failed || err == nil || errors.Is(err, ErrUnknown) || errors.Is(err, ErrSubmissionUncertain) || observation.Matched {
		t.Fatalf("definite process start failure was reported as uncertain: observation=%+v err=%v", observation, err)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 0 {
		t.Fatal("pueue add ran despite the supervisor process failing to start")
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestStopWithoutSavedTargetNeverReconcilesOrMutates(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	record := createSubmission(t, fixture, fake.client.Binding())
	writeStatusFor(t, fake, record.RootID, record.TaskID, 7, StateRunning)
	permit := createStopPermit(t, fixture)
	before := len(readInvocations(t, fake.logPath))
	result, err := fake.client.Stop(context.Background(), permit)
	if !errors.Is(err, ErrUnknown) || !result.Requested || result.Attempted || result.Matched || result.NumericTaskID != nil {
		t.Fatalf("missing target was routed: %+v %v", result, err)
	}
	if after := len(readInvocations(t, fake.logPath)); after != before {
		t.Fatalf("missing target triggered a fresh status lookup: before=%d after=%d", before, after)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
}

func prepareTargetedStop(t *testing.T, mode string, state State) (*fakeSupervisor, *taskFixture, *taskdir.StopPermit, *task.SubmitRecord) {
	t.Helper()
	fake := newFakeSupervisor(t, mode)
	fixture := newTaskFixture(t, fake.client.Binding())
	record := createSubmission(t, fixture, fake.client.Binding())
	writeStatusFor(t, fake, record.RootID, record.TaskID, 7, state)
	identity := identityFromRecord(record)
	id := int64(7)
	identity.NumericTaskID = &id
	observation, err := fake.client.Reconcile(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.taskDir.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	permit := createStopPermit(t, fixture)
	return fake, fixture, permit, record
}

func TestTargetedStopRoutesStateAndSeparatesAcknowledgment(t *testing.T) {
	cases := []targetedStopCase{
		{name: "queued acknowledged remove", mode: "ok", state: StateQueued, action: "remove", ack: boolPointer(true)},
		{name: "running acknowledged kill", mode: "ok", state: StateRunning, action: "kill", ack: boolPointer(true)},
		{name: "queued refusal", mode: "stop-refuse", state: StateQueued, action: "remove", ack: boolPointer(false)},
		{name: "queued output uncertainty", mode: "stop-overflow", state: StateQueued, action: "remove", wantError: ErrControlLimit},
		{name: "ended no mutation", mode: "ok", state: StateEnded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runTargetedStopCase(t, tc) })
	}
}

func runTargetedStopCase(t *testing.T, tc targetedStopCase) {
	t.Helper()
	fake, _, permit, _ := prepareTargetedStop(t, tc.mode, tc.state)
	t.Cleanup(func() {
		if err := permit.Release(); err != nil {
			t.Error(err)
		}
	})
	result, err := fake.client.Stop(context.Background(), permit)
	assertTargetedStopError(t, tc, err)
	if result.ObservedState != tc.state || !result.Matched {
		t.Fatalf("wrong stop observation: %+v", result)
	}
	if result.Action != tc.action || result.Attempted != (tc.action != "") {
		t.Fatalf("wrong stop routing: %+v", result)
	}
	assertTargetedStopAcknowledgment(t, tc, result)
	if countCommand(readInvocations(t, fake.logPath), tc.action) != boolToInt(tc.action != "") {
		t.Fatalf("mutation count changed: %+v", readInvocations(t, fake.logPath))
	}
}

func assertTargetedStopError(t *testing.T, tc targetedStopCase, err error) {
	t.Helper()
	if tc.wantError != nil {
		if !errors.Is(err, tc.wantError) {
			t.Fatalf("unexpected stop error: %v", err)
		}
		return
	}
	if tc.mode == "stop-refuse" {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("refusal did not preserve CLI exit error: %v", err)
		}
		return
	}
	if tc.state != StateEnded && err != nil {
		t.Fatal(err)
	}
}

func assertTargetedStopAcknowledgment(t *testing.T, tc targetedStopCase, result StopResult) {
	t.Helper()
	if tc.ack == nil {
		if result.Acknowledged != nil {
			t.Fatalf("uncertain stop acquired acknowledgment: %+v", result)
		}
		return
	}
	if result.Acknowledged == nil || *result.Acknowledged != *tc.ack {
		t.Fatalf("wrong acknowledgment: %+v", result)
	}
}
