package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func supervisedPathProject(t *testing.T) (*taskdir.PathProjectCustody, string, string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "native"))
	require(t, err)
	t.Cleanup(func() { require(t, store.Close()) })
	require(t, store.EnableWorkspaceConnectorContext(t.Context(), true, true))
	root, err := store.RootTokenContext(t.Context())
	require(t, err)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	path := filepath.Join(parent, "project")
	require(t, os.Mkdir(path, 0o700))
	project, err := taskdir.ObservePathProjectContext(t.Context(), path)
	require(t, err)
	workspace, err := taskdir.ProjectToken(project)
	require(t, err)
	custody, err := store.NewPathProjectCustody(t.Context(), task.ConnectorContext{
		RootToken: root, Project: project, WorkspaceConstraint: &task.WorkspaceConstraint{Token: workspace, RelativeProject: "."},
	})
	require(t, err)
	return custody, path, store.Root
}

func TestSupervisedProjectRefusesRetargetBeforeNativeStart(t *testing.T) {
	custody, path, _ := supervisedPathProject(t)
	lifetime := &connectorProjectGateLifetime{}
	starts := 0
	workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Project: custody, Lifetime: lifetime, Stopper: supervisedContextNoExpiry(t)}, func(scope PreflightScope) error {
		require(t, os.Rename(path, path+"-saved"))
		require(t, os.Symlink(t.TempDir(), path))
		cmd := &exec.Cmd{Dir: path}
		return scope.StartCommand(cmd, func() error { starts++; return nil })
	})
	if workErr == nil || stopErr != nil || starts != 0 || lifetime.projectStarts.Load() != 0 || lifetime.ordinaryStarts.Load() != 0 {
		t.Fatalf("retargeted scope reached native Start: starts=%d work=%v stop=%v", starts, workErr, stopErr)
	}
}

func TestSupervisedProjectRunsReplacementThroughRealChildGate(t *testing.T) {
	custody, path, _ := supervisedPathProject(t)
	executable, err := os.Executable()
	require(t, err)
	executable, err = filepath.EvalSymlinks(executable)
	require(t, err)
	evidence := filepath.Join(t.TempDir(), "cwd.json")
	lifetime := &connectorProjectGateLifetime{runner: executable}
	workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Project: custody, Lifetime: lifetime, Stopper: supervisedContextNoExpiry(t)}, func(scope PreflightScope) error {
		require(t, os.Rename(path, path+"-saved"))
		require(t, os.Mkdir(path, 0o700))
		cmd := exec.Command(executable, "-test.run=^TestConnectorCwdHelper$")
		cmd.Dir, cmd.Env = path, append(os.Environ(), "DELEGATE_CONNECTOR_CWD_EVIDENCE="+evidence)
		if startErr := scope.StartCommand(cmd, cmd.Start); startErr != nil {
			return startErr
		}
		return errors.Join(cmd.Wait(), scope.FinishCommand(cmd))
	})
	if workErr != nil || stopErr != nil || lifetime.projectStarts.Load() != 1 || lifetime.ordinaryStarts.Load() != 0 {
		t.Fatalf("path replacement did not use project lifetime: work=%v stop=%v", workErr, stopErr)
	}
	data, err := os.ReadFile(evidence)
	require(t, err)
	var observed connectorCwdIdentity
	require(t, json.Unmarshal(data, &observed))
	want, err := connectorCwdStat(path)
	require(t, err)
	if observed != want {
		t.Fatalf("actual child cwd=%+v, want current selection %+v", observed, want)
	}
}

func TestSupervisedProjectCannotBypassLifetimeOrCwd(t *testing.T) {
	for _, name := range []string{"missing lifetime", "ordinary lifetime", "wrong cwd"} {
		t.Run(name, func(t *testing.T) {
			custody, path, _ := supervisedPathProject(t)
			var lifetime Lifetime
			switch name {
			case "ordinary lifetime":
				lifetime = recordingLifetime{events: &eventLog{}}
			case "wrong cwd":
				lifetime = &connectorProjectGateLifetime{}
				path = t.TempDir()
			}
			starts := 0
			workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Project: custody, Lifetime: lifetime, Stopper: supervisedContextNoExpiry(t)}, func(scope PreflightScope) error {
				return scope.StartCommand(&exec.Cmd{Dir: path}, func() error { starts++; return nil })
			})
			if workErr == nil || stopErr != nil || starts != 0 {
				t.Fatalf("guard bypassed: starts=%d work=%v stop=%v", starts, workErr, stopErr)
			}
		})
	}
}

func TestOrdinaryPreflightPreservesProviderHomeHelper(t *testing.T) {
	custody, _, _ := supervisedPathProject(t)
	lifetime := &connectorProjectGateLifetime{}
	starts := 0
	scope := PreflightScope{ctx: t.Context(), project: custody, lifetime: lifetime, start: func(start func() error) error { return start() }}
	require(t, scope.StartCommand(&exec.Cmd{Dir: t.TempDir()}, func() error { starts++; return nil }))
	if starts != 1 || lifetime.ordinaryStarts.Load() != 1 || lifetime.projectStarts.Load() != 0 {
		t.Fatal("ordinary provider-home helper changed its existing lifetime route")
	}
	require(t, lifetime.CloseTask(context.WithoutCancel(t.Context())))
}

func TestSupervisedProjectChecksRootAfterLifetimeClosure(t *testing.T) {
	custody, _, root := supervisedPathProject(t)
	lifetime := supervisedContextLifetime{close: func(context.Context) error {
		if err := os.Rename(root, root+"-held"); err != nil {
			return err
		}
		return os.Mkdir(root, 0o700)
	}}
	workErr, stopErr := RunSupervised(time.Now().Add(time.Minute), SupervisedOptions{Project: custody, Lifetime: lifetime, Stopper: supervisedContextNoExpiry(t)}, func(PreflightScope) error {
		return nil
	})
	if !errors.Is(workErr, task.ErrIdentityMismatch) || stopErr != nil {
		t.Errorf("root replacement during closure became successful facts: work=%v stop=%v", workErr, stopErr)
	}
	require(t, os.Remove(root))
	require(t, os.Rename(root+"-held", root))
}
