package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// recordingBinder stands in for an owner-installed helper binder. It records
// each request with its context and either refuses the failAt-th request
// (1-based), returns no command when empty is set, or binds through a private
// copy so the returned view is observable and removed by Command.Close.
type recordingBinder struct {
	mu       sync.Mutex
	failAt   int
	empty    bool
	requests []verifiedexec.CommandRequest
	contexts []context.Context
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
	if b.empty {
		return nil, nil
	}
	return verifiedexec.NewCommandFromRequest(request)
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
