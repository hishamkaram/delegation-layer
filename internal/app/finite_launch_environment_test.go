package app

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type operationalCommandLifetime struct {
	seen    [][]string
	refusal error
}

func (l *operationalCommandLifetime) StartCommand(command *exec.Cmd, _ func() error) error {
	l.seen = append(l.seen, slices.Clone(command.Env))
	return l.refusal
}
func (*operationalCommandLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (*operationalCommandLifetime) Terminate(context.Context) error                { return nil }
func (*operationalCommandLifetime) CloseTask(context.Context) error                { return nil }

type operationalCommandStopper struct{}

func (operationalCommandStopper) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	return func(context.Context) error { return nil }, nil
}

func TestFiniteWorkspaceCommandFinalEnvironment(t *testing.T) {
	for _, selection := range []string{"explicit", "empty", "inherited"} {
		t.Run(selection, func(t *testing.T) {
			a, b := operationalTestStore(t), operationalTestStore(t)
			captured, selected, expected := finiteWorkspaceSelection(t, a, b, selection)
			refusal := errors.New("stop before native command birth")
			lifetime := &operationalCommandLifetime{refusal: refusal}
			workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: operationalCommandStopper{}, Lifetime: lifetime, PrepareEnvironment: operationalEnvironmentPreparation(b)}, func(scope execution.PreflightScope) error {
				command := &exec.Cmd{Path: "/not-started", Args: []string{"not-started"}, Dir: b.Root, Env: selected}
				return (workspaceCommandRunner{scope: scope, environment: captured}).Run(scope.Context(), command)
			})
			if !errors.Is(workErr, refusal) || stopErr != nil || len(lifetime.seen) != 1 {
				t.Fatalf("scoped command did not reach exact refusal: %v %v", workErr, stopErr)
			}
			got := lifetime.seen[0]
			assertOperationalOrigin(t, got, b)
			if !slices.Equal(withoutOperationalTestOrigin(got), expected) {
				t.Fatal("scoped command changed selected non-origin environment")
			}
		})
	}
}

func TestFiniteCommandRootReplacementRefusesBeforeStart(t *testing.T) {
	store := operationalTestStore(t)
	prepare := operationalEnvironmentPreparation(store)
	replaceOperationalTestRoot(t, store)
	lifetime := &operationalCommandLifetime{refusal: errors.New("unexpected native boundary")}
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: operationalCommandStopper{}, Lifetime: lifetime, PrepareEnvironment: prepare}, func(scope execution.PreflightScope) error {
		command := &exec.Cmd{Path: "/not-started", Args: []string{"not-started"}, Dir: store.Root, Env: []string{}}
		return (workspaceCommandRunner{scope: scope, environment: []string{}}).Run(scope.Context(), command)
	})
	if workErr == nil || stopErr != nil || len(lifetime.seen) != 0 {
		t.Fatalf("replaced root reached native boundary: %v %v starts=%d", workErr, stopErr, len(lifetime.seen))
	}
}

func TestFiniteGuardianWorkerReceivesCapturedOrigin(t *testing.T) {
	store := operationalTestStore(t)
	worker, err := newLifetimeWorkerCommand("/not-started", store.Root, runnerArguments{})
	if err != nil {
		t.Fatal(err)
	}
	captured := []string{"CUSTOM=worker", "DELEGATION_LAYER_PROCESS_SCOPE=old-finite-owner"}
	if err = prepareLifetimeWorkerEnvironment(t.Context(), store, worker, captured); err != nil {
		t.Fatal(err)
	}
	assertOperationalOrigin(t, worker.Env, store)
	if !slices.Equal(withoutOperationalTestOrigin(worker.Env), execution.InheritedCommandEnvironment(captured, worker.Dir)) {
		t.Fatal("guardian worker lost captured entries before guardian configuration")
	}
	// This checks command preparation only. Guardian owns replacement of its
	// finite marker and the actual child lifecycle; Origin grants neither.
}

func TestFiniteGuardianWorkerRootReplacementRefuses(t *testing.T) {
	store := operationalTestStore(t)
	worker, err := newLifetimeWorkerCommand("/not-started", store.Root, runnerArguments{})
	if err != nil {
		t.Fatal(err)
	}
	replaceOperationalTestRoot(t, store)
	if err = prepareLifetimeWorkerEnvironment(t.Context(), store, worker, []string{}); err == nil || worker.Env != nil {
		t.Fatal("guardian prepared a worker from a replaced root")
	}
}

func finiteWorkspaceSelection(t *testing.T, a, b *taskdir.Store, selection string) (captured, selected, expected []string) {
	t.Helper()
	old, err := infrastructure.NewOrigin(a.Root, a.RootID)
	if err != nil {
		t.Fatal(err)
	}
	captured = []string{"SELECTED=inherited", "DELEGATION_LAYER_PROCESS_SCOPE=finite-owner"}
	captured, err = infrastructure.WithOrigin(captured, old)
	if err != nil {
		t.Fatal(err)
	}
	selected = []string{"SELECTED=explicit", "CUSTOM=value", "RAW=\xff", "DELEGATION_LAYER_PROCESS_SCOPE=finite-owner"}
	if selection == "empty" {
		return captured, []string{}, []string{}
	}
	if selection == "inherited" {
		selected = nil
		expected = withoutOperationalTestOrigin(execution.InheritedCommandEnvironment(captured, b.Root))
		return captured, selected, expected
	}
	expected = slices.Clone(selected)
	selected, err = infrastructure.WithOrigin(selected, old)
	if err != nil {
		t.Fatal(err)
	}
	return captured, selected, expected
}

func TestFiniteProviderUsesOwningStoreAfterFinalPreflight(t *testing.T) {
	for _, selection := range []string{"explicit", "empty", "inherited", "refreshed"} {
		t.Run(selection, func(t *testing.T) {
			assertFiniteProviderSelection(t, selection)
		})
	}
}

func assertFiniteProviderSelection(t *testing.T, selection string) {
	t.Helper()
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	prior := operationalTestStore(t)
	profile := fixtureLaunchProfile(t, td, request)
	captured, selected, expected := finiteWorkspaceSelection(t, prior, store, providerSelectionBase(selection))
	profile.Plan.Environment = selected
	if selection == "inherited" {
		expected = withoutOperationalTestOrigin(execution.InheritedCommandEnvironment(captured, profile.Plan.Directory))
	}
	original := slices.Clone(selected)
	refreshed := profile
	if selection == "refreshed" {
		refreshed.Plan.Environment = []string{"SELECTED=refreshed", "DELEGATION_LAYER_PROCESS_SCOPE=finite-owner"}
		expected = slices.Clone(refreshed.Plan.Environment)
	}
	refusal := errors.New("stop at ordinary provider native boundary")
	var seen []string
	starts := 0
	deps := Dependencies{ResourceChecker: fixtureResources, Environment: captured, PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) { return refreshed, nil }, ExecutionHooks: execution.Hooks{Start: func(command *exec.Cmd) error { starts++; seen = slices.Clone(command.Env); return refusal }}}
	result := runProvider(newResponse("dispatch"), store, td, request, profile, nil, nil, deps)
	if result.err != nil || result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() || starts != 1 {
		t.Fatalf("provider boundary outcome: code=%d starts=%d err=%v", result.code, starts, result.err)
	}
	assertOperationalOrigin(t, seen, store)
	if !slices.Equal(withoutOperationalTestOrigin(seen), expected) {
		t.Fatal("provider used stale or ambient environment after final preflight")
	}
	if !slices.Equal(profile.Plan.Environment, original) || (profile.Plan.Environment == nil) != (original == nil) {
		t.Fatal("provider overlay changed admitted plan")
	}
}

func providerSelectionBase(selection string) string {
	if selection == "refreshed" {
		return "explicit"
	}
	return selection
}

func TestFiniteProviderNilStoreCannotDisableOriginChecks(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	profile := fixtureLaunchProfile(t, td, request)
	starts := 0
	deps := Dependencies{ResourceChecker: fixtureResources, Environment: []string{}, PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) { return profile, nil }, ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }}}
	result := runProvider(newResponse("dispatch"), nil, td, request, profile, nil, nil, deps)
	if starts != 0 || result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("nil borrowed Store bypassed Origin checks: starts=%d", starts)
	}
}
