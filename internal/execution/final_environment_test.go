package execution

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These real permit/capture fixtures stop at the native Start hook. They
// establish environment transport and refusal, not provider or owner authority.
func TestFinalEnvironmentUsesFinalPlanSelection(t *testing.T) {
	for _, selection := range []string{"explicit", "empty", "inherited", "refreshed"} {
		t.Run(selection, func(t *testing.T) {
			assertFinalPlanEnvironmentSelection(t, selection)
		})
	}
}

func assertFinalPlanEnvironmentSelection(t *testing.T, selection string) {
	t.Helper()
	environment := []string{"SELECTED=explicit"}
	if selection == "empty" {
		environment = []string{}
	}
	if selection == "inherited" || selection == "refreshed" {
		environment = nil
	}
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", func(_ *task.MetaRecord, p *Plan) { p.Environment = environment })
	original := slices.Clone(plan.Environment)
	inherited := []string{"SELECTED=inherited", "PWD=old"}
	expected := slices.Clone(environment)
	if environment == nil {
		expected = InheritedCommandEnvironment(inherited, plan.Directory)
	}
	stop := errors.New("stop at final environment transport")
	calls := 0
	var seen []string
	opts := Options{InheritedEnvironment: inherited, PrepareEnvironment: func(ctx context.Context, selected []string) ([]string, error) {
		calls++
		if ctx == nil || ctx.Err() != nil {
			return nil, errors.New("missing active environment context")
		}
		return append(selected, "FINAL=prepared"), nil
	}, Hooks: Hooks{Start: func(command *exec.Cmd) error { seen = slices.Clone(command.Env); return stop }}}
	if selection == "refreshed" {
		expected = []string{"SELECTED=refreshed"}
		opts.PreflightPlan = func(_ PreflightScope, admitted Plan) (Plan, error) {
			admitted.Environment = []string{"SELECTED=refreshed"}
			return admitted, nil
		}
	}
	result := Run(td, permit, plan, opts)
	if calls != 1 || !slices.Equal(seen, append(expected, "FINAL=prepared")) {
		t.Fatalf("final environment selection mismatch: hook calls=%d", calls)
	}
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatal("Start refusal was not sealed")
	}
	if string(payload(t, td, result.Outcome)) != "start_failed: "+stop.Error() {
		t.Fatal("Start refusal cause lost")
	}
	if !slices.Equal(plan.Environment, original) || (plan.Environment == nil) != (original == nil) {
		t.Fatal("environment preparation changed admitted plan")
	}
}

func TestFinalEnvironmentRefusalPreventsNativeStart(t *testing.T) {
	for _, nilResult := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "nil result"}[nilResult], func(t *testing.T) {
			td, permit, plan, _ := fixtureTask(t, "echo")
			refused := errors.New("environment preparation refused")
			starts := 0
			result := Run(td, permit, plan, Options{PrepareEnvironment: func(context.Context, []string) ([]string, error) {
				if nilResult {
					return nil, nil
				}
				return nil, refused
			}, Hooks: Hooks{Start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }}})
			if starts != 0 || result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
				t.Fatalf("environment refusal reached Start: starts=%d err=%v", starts, result.Error)
			}
			require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
			if !nilResult && string(payload(t, td, result.Outcome)) != "start_failed: "+refused.Error() {
				t.Fatal("environment cause lost")
			}
		})
	}
}

func TestFinalEnvironmentSupervisedContextAndOwnership(t *testing.T) {
	clock := newAdvancingClock()
	selected := []string{"RAW=\xff", "PWD=first", "PWD=second"}
	original := slices.Clone(selected)
	var callbackContext context.Context
	var captured PreflightScope
	var returned []string
	workErr, stopErr := RunSupervised(clock.Now().Add(time.Minute), SupervisedOptions{Clock: clock, Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) { return nil, errors.New("unexpected stop") }), PrepareEnvironment: func(ctx context.Context, environment []string) ([]string, error) {
		callbackContext = ctx
		environment[0] = "CHANGED=owned"
		returned = environment
		return environment, nil
	}}, func(scope PreflightScope) error {
		captured = scope
		environment, err := scope.PrepareEnvironment(selected)
		if err != nil {
			return err
		}
		returned[0] = "AFTER=callback"
		if environment[0] != "CHANGED=owned" || !slices.Equal(selected, original) {
			t.Fatal("environment preparation aliased selected or returned data")
		}
		return nil
	})
	if workErr != nil || stopErr != nil {
		t.Fatalf("supervised environment preparation: %v %v", workErr, stopErr)
	}
	if callbackContext != captured.Context() || callbackContext.Err() == nil {
		t.Fatal("hook lost the real expiring scope context")
	}
	if _, err := captured.PrepareEnvironment(selected); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired scope prepared a new launch environment: %v", err)
	}
	for _, environment := range [][]string{nil, {}} {
		got, err := prepareLaunchEnvironment(context.Background(), environment, nil)
		if err != nil || (got == nil) != (environment == nil) {
			t.Fatal("nil hook changed selection semantics")
		}
	}
}

func TestFinalEnvironmentZeroConfiguredScopeRefuses(t *testing.T) {
	calls := 0
	scope := PreflightScope{prepareEnvironment: func(context.Context, []string) ([]string, error) { calls++; return []string{}, nil }}
	if _, err := scope.PrepareEnvironment([]string{}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("zero scope prepared environment: calls=%d err=%v", calls, err)
	}
}

func TestFinalEnvironmentConfiguredHookRequiresResolvedSelection(t *testing.T) {
	calls := 0
	prepare := func(_ context.Context, selected []string) ([]string, error) {
		calls++
		return append(selected, "ORIGIN=selected"), nil
	}
	if got, err := prepareLaunchEnvironment(t.Context(), nil, prepare); err == nil || got != nil || calls != 0 {
		t.Fatalf("unresolved inheritance became origin-only: calls=%d err=%v", calls, err)
	}
	got, err := prepareLaunchEnvironment(t.Context(), []string{}, prepare)
	if err != nil || calls != 1 || !slices.Equal(got, []string{"ORIGIN=selected"}) {
		t.Fatalf("explicit empty selection refused: calls=%d err=%v", calls, err)
	}
}
