package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// recordingBinder stands in for an owner-installed binder. It records each
// request with its context and either refuses the failAt-th request (1-based),
// refuses the first request with the error initial returns, returns no command
// when empty is set, or binds through a private copy so the returned view is
// observable and removed by Command.Close.
type recordingBinder struct {
	mu       sync.Mutex
	failAt   int
	initial  func(verifiedexec.CommandRequest) error
	empty    bool
	requests []verifiedexec.CommandRequest
	contexts []context.Context
	commands []*verifiedexec.Command
}

var errRecordingBinder = errors.New("supplied binder refused")

func (b *recordingBinder) BindCommand(ctx context.Context, request verifiedexec.CommandRequest) (*verifiedexec.Command, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	b.contexts = append(b.contexts, ctx)
	if len(b.requests) == b.failAt {
		return nil, errRecordingBinder
	}
	if len(b.requests) == 1 && b.initial != nil {
		return nil, b.initial(request)
	}
	if b.empty {
		return nil, nil
	}
	command, err := verifiedexec.NewCommandFromRequest(request)
	if err == nil {
		b.commands = append(b.commands, command)
	}
	return command, err
}

func (b *recordingBinder) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

type binderTaskFixture struct {
	td     *taskdir.TaskDir
	permit *taskdir.StartPermit
	plan   Plan
}

// binderTask selects a private TMPDIR for every view of the task, so a view
// made outside the supplied binder, or one left behind, is observable.
func binderTask(t *testing.T) (binderTaskFixture, string) {
	t.Helper()
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	td, permit, plan, _ := fixtureTask(t, "echo")
	plan.Environment = append(slices.Clone(plan.Environment), "TMPDIR="+temporary)
	return binderTaskFixture{td: td, permit: permit, plan: plan}, temporary
}

func unexpectedBinderStop(t *testing.T) Stopper {
	return budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		t.Error("completed inspection requested a stop")
		return nil, errors.New("unexpected stop")
	})
}

func assertNoTaskViews(t *testing.T, temporary string) {
	t.Helper()
	entries, err := os.ReadDir(temporary)
	require(t, err)
	if len(entries) != 0 {
		t.Fatalf("verified views remain in %s: %v", temporary, entries)
	}
}

func TestRunBindsInitialAndRefreshedPlanThroughSuppliedBinder(t *testing.T) {
	fixture, temporary := binderTask(t)
	refreshed := fixture.plan
	refreshed.Environment = append(slices.Clone(fixture.plan.Environment), "DELEGATE_REFRESHED_PLAN=1")
	binder := &recordingBinder{}
	var preflightContext context.Context
	var started *exec.Cmd
	result := Run(fixture.td, fixture.permit, fixture.plan, Options{
		Binder: binder,
		PreflightPlan: func(scope PreflightScope, _ Plan) (Plan, error) {
			// A native inspection inside the task preflight binds through the
			// same supplied binder with the scope's deadline context.
			preflightContext = scope.Context()
			command, err := scope.BindCommand(verifiedexec.CommandRequest{
				Executable: refreshed.Executable, SHA256: refreshed.ExecutableSHA256,
				Directory: refreshed.Directory, Environment: refreshed.Environment, Args: []string{"inspect"},
			})
			if err != nil {
				return Plan{}, err
			}
			return refreshed, command.Close()
		},
		Hooks: Hooks{Start: func(cmd *exec.Cmd) error {
			started = cmd
			return cmd.Start()
		}},
	})
	assertSuccess(t, result)
	assertInitialPreflightAndRefreshedBindings(t, binder, fixture.plan, refreshed, preflightContext)
	if started == nil || started != binder.commands[2].Cmd {
		t.Fatal("provider did not start the command bound for the refreshed plan")
	}
	assertNoTaskViews(t, temporary)
}

// assertInitialPreflightAndRefreshedBindings checks the three bindings of a
// refreshed task: the admitted plan before the budget, the preflight's native
// command with the scope context, and the refreshed plan with the budget's
// deadline context.
func assertInitialPreflightAndRefreshedBindings(t *testing.T, binder *recordingBinder, admitted, refreshed Plan, preflightContext context.Context) {
	t.Helper()
	if binder.calls() != 3 {
		t.Fatalf("binder calls = %d, want initial, preflight and refreshed", binder.calls())
	}
	initial, preflight, rebound := binder.requests[0], binder.requests[1], binder.requests[2]
	for _, request := range []verifiedexec.CommandRequest{initial, rebound} {
		if request.Executable != admitted.Executable || request.SHA256 != admitted.ExecutableSHA256 ||
			request.Directory != admitted.Directory || !slices.Equal(request.Args, admitted.Arguments) {
			t.Fatalf("binder request lost the launch identity: %+v", request)
		}
	}
	if !slices.Equal(initial.Environment, admitted.Environment) || !slices.Equal(rebound.Environment, refreshed.Environment) {
		t.Fatal("binder requests did not carry the initial and refreshed launch environments")
	}
	if !slices.Equal(preflight.Args, []string{"inspect"}) || binder.contexts[1] != preflightContext {
		t.Fatal("preflight scope did not bind through the supplied binder with its context")
	}
	if _, finite := binder.contexts[0].Deadline(); finite {
		t.Fatal("initial binding claimed a deadline before the budget exists")
	}
	if deadline, finite := binder.contexts[2].Deadline(); !finite || deadline.IsZero() {
		t.Fatal("refreshed binding did not receive the budget deadline context")
	}
}

func TestRunRefusesSuppliedBinderFaultWithoutStartOrFallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		binder  *recordingBinder
		refresh bool
		calls   int
		want    error
	}{
		{name: "initial", binder: &recordingBinder{failAt: 1}, calls: 1, want: errRecordingBinder},
		{name: "initial-refreshable", binder: &recordingBinder{failAt: 1}, refresh: true, calls: 1, want: errRecordingBinder},
		{name: "initial-without-command", binder: &recordingBinder{empty: true}, calls: 1, want: errInvalidBoundCommand},
		{name: "refreshed", binder: &recordingBinder{failAt: 2}, refresh: true, calls: 2, want: errRecordingBinder},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, temporary := binderTask(t)
			options := Options{Binder: test.binder}
			if test.refresh {
				refreshed := fixture.plan
				refreshed.Environment = append(slices.Clone(fixture.plan.Environment), "DELEGATE_REFRESHED_PLAN=1")
				options.PreflightPlan = func(PreflightScope, Plan) (Plan, error) { return refreshed, nil }
			}
			starts := 0
			options.Hooks.Start = func(cmd *exec.Cmd) error {
				starts++
				return cmd.Start()
			}
			result := Run(fixture.td, fixture.permit, fixture.plan, options)
			require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
			if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
				t.Fatalf("binder refusal did not publish a rejection: %+v", result)
			}
			if starts != 0 || test.binder.calls() != test.calls {
				t.Fatalf("starts=%d binder calls=%d, want 0 and %d", starts, test.binder.calls(), test.calls)
			}
			exit, err := os.ReadFile(filepath.Join(fixture.td.Dir, "provider.exit"))
			require(t, err)
			var seal task.ProviderExitRecord
			require(t, task.DecodeStrict(exit, &seal))
			if seal.InvocationState != task.InvocationStartFailed || seal.Error != test.want.Error() {
				t.Fatalf("binder refusal sealed %+v, want start failure %q", seal, test.want)
			}
			assertNoTaskViews(t, temporary)
		})
	}
}

func TestSupervisedScopeBindsThroughSuppliedBinder(t *testing.T) {
	executable, err := os.Executable()
	require(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require(t, err)
	data, err := os.ReadFile(executable)
	require(t, err)
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	request := verifiedexec.CommandRequest{
		Executable: executable, SHA256: task.ComputeSHA256(data),
		Directory: temporary, Environment: []string{"TMPDIR=" + temporary}, Args: []string{"-test.run=^$"},
	}
	for _, test := range []struct {
		name   string
		binder *recordingBinder
		want   error
	}{
		{name: "binds", binder: &recordingBinder{}},
		{name: "refuses", binder: &recordingBinder{failAt: 1}, want: errRecordingBinder},
	} {
		t.Run(test.name, func(t *testing.T) {
			var scopeContext context.Context
			workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Stopper: unexpectedBinderStop(t), Binder: test.binder}, func(scope PreflightScope) error {
				scopeContext = scope.Context()
				command, bindErr := scope.BindCommand(request)
				if bindErr != nil {
					return bindErr
				}
				if command.Cmd.Args[0] != executable || !slices.Equal(command.Cmd.Args[1:], request.Args) {
					t.Errorf("bound command arguments = %q", command.Cmd.Args)
				}
				return command.Close()
			})
			if !errors.Is(workErr, test.want) || (test.want == nil && workErr != nil) || stopErr != nil {
				t.Fatalf("work=%v stop=%v, want %v", workErr, stopErr, test.want)
			}
			if test.binder.calls() != 1 || test.binder.contexts[0] != scopeContext || !slices.Equal(test.binder.requests[0].Args, request.Args) {
				t.Fatal("supervised scope did not bind through the supplied binder with its context")
			}
			assertNoTaskViews(t, temporary)
		})
	}
}

func TestScopeWithoutBinderKeepsPrivateCopy(t *testing.T) {
	executable, err := os.Executable()
	require(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require(t, err)
	data, err := os.ReadFile(executable)
	require(t, err)
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Stopper: unexpectedBinderStop(t)}, func(scope PreflightScope) error {
		command, bindErr := scope.BindCommand(verifiedexec.CommandRequest{
			Executable: executable, SHA256: task.ComputeSHA256(data),
			Directory: temporary, Environment: []string{"TMPDIR=" + temporary},
		})
		if bindErr != nil {
			return bindErr
		}
		entries, readErr := os.ReadDir(temporary)
		if readErr != nil || len(entries) != 1 || command.Cmd.Path == executable {
			t.Errorf("default binding is not a private copy under TMPDIR: entries=%v path=%q err=%v", entries, command.Cmd.Path, readErr)
		}
		return command.Close()
	})
	require(t, errors.Join(workErr, stopErr))
	assertNoTaskViews(t, temporary)
}
