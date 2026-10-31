package execution

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type binderTaskFixture struct {
	td     *taskdir.TaskDir
	permit *taskdir.StartPermit
	plan   Plan
}

// binderTask selects a private TMPDIR so every helper view is observable.
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

func helperCommandRequest(t *testing.T, temporary string) verifiedexec.CommandRequest {
	t.Helper()
	executable, err := os.Executable()
	require(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require(t, err)
	data, err := os.ReadFile(executable)
	require(t, err)
	return verifiedexec.CommandRequest{
		Executable: executable, SHA256: task.ComputeSHA256(data),
		Directory: temporary, Environment: []string{"TMPDIR=" + temporary}, Args: []string{"-test.run=^$"},
	}
}

func TestScopeKeepsVerifiedPrivateCopy(t *testing.T) {
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	request := helperCommandRequest(t, temporary)
	workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Stopper: unexpectedBinderStop(t)}, func(scope PreflightScope) error {
		command, bindErr := scope.BindCommand(request)
		if bindErr != nil {
			return bindErr
		}
		entries, readErr := os.ReadDir(temporary)
		if readErr != nil || len(entries) != 1 || command.Cmd.Path == request.Executable {
			t.Errorf("binding is not a private copy under TMPDIR: entries=%v path=%q err=%v", entries, command.Cmd.Path, readErr)
		}
		data, readErr := os.ReadFile(command.Cmd.Path)
		if readErr != nil || task.ComputeSHA256(data) != request.SHA256 {
			t.Errorf("helper copy does not hold the admitted bytes: %v", readErr)
		}
		if command.Cmd.Args[0] != request.Executable || !slices.Equal(command.Cmd.Args[1:], request.Args) {
			t.Errorf("bound command lost its arguments: %q", command.Cmd.Args)
		}
		return command.Close()
	})
	require(t, errors.Join(workErr, stopErr))
	assertNoTaskViews(t, temporary)
}
