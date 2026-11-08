package worker

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// workspaceCommandRunner borrows the existing scope; Git never starts, waits,
// signals, or creates a second deadline owner.
type workspaceCommandRunner struct {
	scope       execution.PreflightScope
	environment []string
}

func (r workspaceCommandRunner) Run(ctx context.Context, command *exec.Cmd) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if command == nil || !filepath.IsAbs(command.Dir) {
		return task.ErrIdentityMismatch
	}
	// Git and verification have already resolved their selected PATH. This
	// overlay rechecks the root before Start; it does not repeat that lookup.
	selected := command.Env
	if selected == nil && r.environment != nil {
		selected = execution.InheritedCommandEnvironment(r.environment, command.Dir)
	}
	environment, err := r.scope.PrepareEnvironment(selected)
	if err != nil {
		return err
	}
	command.Env = environment
	return execution.RunScopedCommand(r.scope, command, execution.CommandHooks{})
}
