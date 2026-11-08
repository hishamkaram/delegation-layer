package worker

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type commandTestLifetime struct {
	seen    [][]string
	refusal error
}

func (l *commandTestLifetime) StartCommand(c *exec.Cmd, _ func() error) error {
	l.seen = append(l.seen, slices.Clone(c.Env))
	return l.refusal
}
func (*commandTestLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (*commandTestLifetime) Terminate(context.Context) error                { return nil }
func (*commandTestLifetime) CloseTask(context.Context) error                { return nil }

type commandTestStopper struct{}

func (commandTestStopper) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	return func(context.Context) error { return nil }, nil
}

func commandTestStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(supervisorfixture.CanonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

// The scope owns this overlay; the command runner must consume it unchanged.
func commandEnvironmentPreparation(store *taskdir.Store) func(context.Context, []string) ([]string, error) {
	return func(ctx context.Context, selected []string) ([]string, error) {
		pin, err := store.PinRootContext(ctx)
		if err != nil {
			return nil, err
		}
		origin, err := infrastructure.NewOrigin(pin.Root(), pin.RootID())
		if err != nil {
			return nil, err
		}
		return infrastructure.WithOrigin(selected, origin)
	}
}

func assertCommandOrigin(t *testing.T, environment []string, store *taskdir.Store) {
	t.Helper()
	origin, found, err := infrastructure.ReadOrigin(environment)
	if err != nil || !found || origin.Root() != store.Root || origin.RootID() != store.RootID {
		t.Fatal("launch did not carry its actual owning root locator")
	}
}

func withoutCommandOrigin(environment []string) []string {
	result := []string{}
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != infrastructure.OriginEnvironment {
			result = append(result, entry)
		}
	}
	return result
}

func TestFiniteWorkspaceCommandFinalEnvironment(t *testing.T) {
	for _, selection := range []string{"explicit", "empty", "inherited"} {
		t.Run(selection, func(t *testing.T) {
			a, b := commandTestStore(t), commandTestStore(t)
			captured, selected, expected := commandEnvironmentSelection(t, a, b, selection)
			refusal := errors.New("stop before native command birth")
			lifetime := &commandTestLifetime{refusal: refusal}
			workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), execution.SupervisedOptions{Stopper: commandTestStopper{}, Lifetime: lifetime, PrepareEnvironment: commandEnvironmentPreparation(b)}, func(scope execution.PreflightScope) error {
				command := &exec.Cmd{Path: "/not-started", Args: []string{"not-started"}, Dir: b.Root, Env: selected}
				return (workspaceCommandRunner{scope: scope, environment: captured}).Run(scope.Context(), command)
			})
			if !errors.Is(workErr, refusal) || stopErr != nil || len(lifetime.seen) != 1 {
				t.Fatalf("scoped command did not reach exact refusal: %v %v", workErr, stopErr)
			}
			got := lifetime.seen[0]
			assertCommandOrigin(t, got, b)
			if !slices.Equal(withoutCommandOrigin(got), expected) {
				t.Fatal("scoped command changed selected non-origin environment")
			}
		})
	}
}

func commandEnvironmentSelection(t *testing.T, a, b *taskdir.Store, selection string) (captured, selected, expected []string) {
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
		expected = withoutCommandOrigin(execution.InheritedCommandEnvironment(captured, b.Root))
		return captured, selected, expected
	}
	expected = slices.Clone(selected)
	selected, err = infrastructure.WithOrigin(selected, old)
	if err != nil {
		t.Fatal(err)
	}
	return captured, selected, expected
}
