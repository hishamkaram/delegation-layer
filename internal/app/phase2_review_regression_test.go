package app

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRunnerStartStateErrorRefusesUnknownBeforeProviderStart(t *testing.T) {
	cases := []struct {
		name      string
		state     pueue.State
		wantError bool
		wantCause error
	}{
		{name: "queued", state: pueue.StateQueued},
		{name: "running", state: pueue.StateRunning},
		{name: "ended", state: pueue.StateEnded, wantError: true},
		{name: "unknown", state: pueue.StateUnknown, wantError: true, wantCause: pueue.ErrUnknown},
		{name: "unsupported", state: pueue.State("paused"), wantError: true, wantCause: pueue.ErrUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runnerStartStateError(tc.state)
			if tc.wantError != (err != nil) {
				t.Fatalf("state %q error=%v, wantError=%t", tc.state, err, tc.wantError)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("state %q error=%v does not preserve %v", tc.state, err, tc.wantCause)
			}
		})
	}
}

func TestStopResponseFromResultPreservesOnlyProvenTermination(t *testing.T) {
	id := int64(7)
	cases := []struct {
		name       string
		state      pueue.State
		numericID  *int64
		terminated bool
	}{
		{name: "ended with target", state: pueue.StateEnded, numericID: &id, terminated: true},
		{name: "ended without target", state: pueue.StateEnded},
		{name: "unknown with target", state: pueue.StateUnknown, numericID: &id},
		{name: "running with target", state: pueue.StateRunning, numericID: &id},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := stopResponseFromResult("request", "budget", pueue.StopResult{ObservedState: tc.state, NumericTaskID: tc.numericID})
			if response.Terminated != tc.terminated {
				t.Fatalf("response terminated=%t, want %t: %+v", response.Terminated, tc.terminated, response)
			}
		})
	}
}

func TestBudgetPreparePersistsBeforeCanceledCallbackAndReleasesPermit(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	meta := prepareAppBudgetRecords(t, td)

	var supervisorEvents atomic.Int32
	stopper := newAppBudgetStopper(t, td, meta, &supervisorEvents)
	deadline := time.Now().Add(time.Minute)
	request := prepareBudgetRequest(t, stopper, deadline)
	saved := readPreparedBudgetRequest(t, td, deadline)

	ctx := canceledBudgetContext()
	assertCanceledBudgetCallback(t, request, ctx)
	assertNoSupervisorEvents(t, &supervisorEvents)
	assertBudgetRequestPreserved(t, td, saved)
	assertBudgetCallbackReleased(t, request, ctx)
	assertNoSupervisorEvents(t, &supervisorEvents)
}

func newAppBudgetStopper(t *testing.T, td *taskdir.TaskDir, meta *task.MetaRecord, supervisorEvents *atomic.Int32) *budgetStopper {
	t.Helper()
	client, err := pueue.NewClient(meta.SupervisorConfig, pueue.Options{Observer: func(pueue.CommandEvent) {
		supervisorEvents.Add(1)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &budgetStopper{client: client, taskDir: td}
}

func prepareBudgetRequest(t *testing.T, stopper *budgetStopper, deadline time.Time) execution.BudgetRequest {
	t.Helper()
	request, err := stopper.PrepareBudget(deadline)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil {
		t.Fatal("budget preparation returned no callback")
	}
	return request
}

func readPreparedBudgetRequest(t *testing.T, td *taskdir.TaskDir, deadline time.Time) *task.StopRequestRecord {
	t.Helper()
	saved, err := td.ReadStopRequest("budget")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Deadline != deadline.UTC().Format(time.RFC3339Nano) || saved.NumericTaskID == nil || *saved.NumericTaskID != 7 {
		t.Fatalf("budget request was not durably bound before callback: %+v", saved)
	}
	return saved
}

func canceledBudgetContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func assertCanceledBudgetCallback(t *testing.T, request execution.BudgetRequest, ctx context.Context) {
	t.Helper()
	if err := request(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled budget callback error=%v", err)
	}
}

func assertNoSupervisorEvents(t *testing.T, supervisorEvents *atomic.Int32) {
	t.Helper()
	if count := supervisorEvents.Load(); count != 0 {
		t.Fatalf("canceled callback invoked supervisor %d times", count)
	}
}

func assertBudgetRequestPreserved(t *testing.T, td *taskdir.TaskDir, saved *task.StopRequestRecord) {
	t.Helper()
	savedAfter, err := td.ReadStopRequest("budget")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(savedAfter, saved) {
		t.Fatalf("canceled callback changed durable request: before=%+v after=%+v", saved, savedAfter)
	}
}

func assertBudgetCallbackReleased(t *testing.T, request execution.BudgetRequest, ctx context.Context) {
	t.Helper()
	if err := request(ctx); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("released budget permit remained usable: %v", err)
	}
}

func prepareAppBudgetRecords(t *testing.T, td *taskdir.TaskDir) *task.MetaRecord {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	submitPermit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = submitPermit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = submitPermit.Release(); err != nil {
		t.Fatal(err)
	}
	submit, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	ref := submit.Supervisor
	receipt := task.SupervisorReceipt{
		SchemaVersion:        task.SchemaVersion,
		RootID:               submit.RootID,
		TaskID:               submit.TaskID,
		SpecSHA256:           submit.SpecSHA256,
		MetaSHA256:           submit.MetaSHA256,
		NumericTaskID:        7,
		Label:                submit.Label,
		ConfigPath:           ref.ConfigPath,
		ConfigDigest:         ref.ConfigDigest,
		Endpoint:             ref.Endpoint,
		ObservedVersion:      ref.ObservedVersion,
		ClientExecutable:     ref.ClientExecutable,
		ClientSHA256:         ref.ClientSHA256,
		ResolvedConfigSHA256: ref.ResolvedConfigSHA256,
	}
	if err = td.RecordSupervisorReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
	return meta
}
