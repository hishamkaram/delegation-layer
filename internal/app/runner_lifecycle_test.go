package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type runnerBudgetLifetime struct {
	terminateCalls atomic.Int32
}

func (*runnerBudgetLifetime) StartCommand(*exec.Cmd, func() error) error { return nil }

func (*runnerBudgetLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }

func (l *runnerBudgetLifetime) Terminate(context.Context) error {
	l.terminateCalls.Add(1)
	return nil
}

func (*runnerBudgetLifetime) CloseTask(context.Context) error { return nil }

func TestApplySavedEnvironmentClearsRecordedEmptyEnvironment(t *testing.T) {
	const key = "DELEGATE_RECORDED_EMPTY_ENV_TEST"
	t.Setenv(key, "ambient")
	restore, err := applySavedEnvironment(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "" {
		t.Fatalf("recorded empty environment retained ambient value %q", got)
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "ambient" {
		t.Fatalf("environment was not restored after explicit empty environment: %q", got)
	}
}

func TestNewMetaPreservesInheritedEnvironmentMarker(t *testing.T) {
	meta := tasklifecycle.NewMeta(task.TaskRecord{}, PreparedProfile{Plan: execution.Plan{}}, task.SupervisorRef{}, "", "", "")
	if meta.Environment != nil {
		t.Fatalf("inherited environment was materialized: %#v", meta.Environment)
	}
	if meta.EnvironmentRecorded {
		t.Fatal("inherited environment was marked as explicitly recorded")
	}

	explicitEmpty := tasklifecycle.NewMeta(task.TaskRecord{}, PreparedProfile{Plan: execution.Plan{Environment: []string{}}}, task.SupervisorRef{}, "", "", "")
	if explicitEmpty.Environment == nil {
		t.Fatal("explicit empty environment lost its distinction from inheritance")
	}
	if !explicitEmpty.EnvironmentRecorded {
		t.Fatal("explicit empty environment was not marked as recorded")
	}
}

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
			err := tasklifecycle.RunnerStartStateError(tc.state)
			if tc.wantError != (err != nil) {
				t.Fatalf("state %q error=%v, wantError=%t", tc.state, err, tc.wantError)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("state %q error=%v does not preserve %v", tc.state, err, tc.wantCause)
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

func TestBudgetLifetimeAcknowledgesWithoutClaimingTerminationObservation(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	meta := prepareAppBudgetRecords(t, td)

	var supervisorEvents atomic.Int32
	lifetimeOwner := &runnerBudgetLifetime{}
	stopper := newAppBudgetStopper(t, td, meta, &supervisorEvents)
	stopper.lifetime = lifetimeOwner
	request := prepareBudgetRequest(t, stopper, time.Now().Add(time.Minute))
	if err := request(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls := lifetimeOwner.terminateCalls.Load(); calls != 1 {
		t.Fatalf("lifetime termination calls = %d, want 1", calls)
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Reply == nil || !records[0].Reply.Acknowledged {
		t.Fatalf("budget stop reply = %+v, want one acknowledged reply", records)
	}
	if records[0].Observation != nil {
		t.Fatalf("worker published guardian-owned termination observation: %+v", records[0].Observation)
	}
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
	return lifecyclefixture.PrepareAppBudgetRecords(t, td)
}
