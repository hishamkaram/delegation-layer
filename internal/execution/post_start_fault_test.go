package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type postStartFaultLifetime struct {
	gate       connectorProjectGateLifetime
	fault      error
	closeFault error
	finishes   atomic.Int32
}

func (l *postStartFaultLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	if err := start(); err != nil {
		return err
	}
	return l.fault
}

func (l *postStartFaultLifetime) StartProjectCommand(command *exec.Cmd, directory *os.File, launch task.ProjectLaunchContext, start func() error) error {
	if err := l.gate.StartProjectCommand(command, directory, launch, start); err != nil {
		return err
	}
	// Deliberately close the borrowed operation file after native Start to inject
	// os.ErrClosed from its owner's deferred selection.Close, without a new seam.
	l.closeFault = directory.Close()
	return l.closeFault
}

func (l *postStartFaultLifetime) FinishCommand(ctx context.Context, command *exec.Cmd) error {
	l.finishes.Add(1)
	if command.ProcessState == nil {
		return task.ErrEvidenceFault
	}
	return l.gate.FinishCommand(ctx, command)
}

func (l *postStartFaultLifetime) Terminate(ctx context.Context) error {
	return l.gate.Terminate(ctx)
}

func (l *postStartFaultLifetime) CloseTask(ctx context.Context) error {
	return l.gate.CloseTask(ctx)
}

func TestProviderPostStartFaultRetainsWaitAndFinishOwnership(t *testing.T) {
	for _, project := range []bool{true, false} {
		t.Run(map[bool]string{true: "project_directory_close", false: "lifetime_control"}[project], func(t *testing.T) {
			assertProviderPostStartFault(t, project)
		})
	}
}

func postStartFaultTask(t *testing.T, project bool) (*taskdir.TaskDir, *taskdir.StartPermit, Plan) {
	t.Helper()
	if project {
		directory, permit, plan, _ := connectorCwdTaskBinding(t, true, true)
		return directory, permit, plan
	}
	directory, permit, plan, _ := fixtureTask(t, "echo")
	return directory, permit, plan
}

func assertProviderPostStartFault(t *testing.T, project bool) {
	t.Helper()
	directory, permit, plan := postStartFaultTask(t, project)
	defer func() { require(t, permit.Release()) }()
	injected := errors.New("control failed after native Start")
	wantFault := injected
	if project {
		wantFault = os.ErrClosed
	}
	lifetime := &postStartFaultLifetime{gate: connectorProjectGateLifetime{runner: plan.Executable}, fault: injected}
	var starts, waits atomic.Int32
	var command *exec.Cmd
	// The pre-fix runner skips Wait on this fault. The finite fixture exits by
	// itself; retain its original command and reap it even on the expected red.
	t.Cleanup(func() {
		if command != nil && command.Process != nil && waits.Load() == 0 {
			if err := command.Wait(); err != nil {
				t.Errorf("join child abandoned by runner: %v", err)
			}
		}
	})
	result := Run(directory, permit, plan, Options{Lifetime: lifetime, Hooks: Hooks{
		Start: func(current *exec.Cmd) error {
			command = current
			starts.Add(1)
			return current.Start()
		},
		Wait: func(current *exec.Cmd) error {
			waits.Add(1)
			return current.Wait()
		},
	}})
	if starts.Load() != 1 || waits.Load() != 1 || lifetime.finishes.Load() != 1 {
		t.Fatalf("post-Start fault lost child ownership: Start=%d Wait=%d FinishCommand=%d", starts.Load(), waits.Load(), lifetime.finishes.Load())
	}
	require(t, lifetime.closeFault)
	if !errors.Is(result.Error, wantFault) || result.Outcome != nil {
		t.Fatalf("post-Start fault was hidden or published: want=%v result=%+v", wantFault, result)
	}
	if command.ProcessState == nil || !command.ProcessState.Exited() || command.ProcessState.ExitCode() != 0 {
		t.Fatalf("finite provider was not successfully joined: %v", command.ProcessState)
	}
	assertPostStartFaultUnsealed(t, directory)
}

func assertPostStartFaultUnsealed(t *testing.T, directory *taskdir.TaskDir) {
	t.Helper()
	inspection, err := directory.Inspect()
	require(t, err)
	if !inspection.StartedExists || inspection.SealExists || inspection.OutcomeExists {
		t.Fatalf("actual native Start was misreported or accepted despite cleanup fault: %+v", inspection)
	}
}
