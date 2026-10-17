package control

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func (c *Client) command(ctx context.Context, consume func() error, args ...string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	cmd, err := c.prepareCommand(args...)
	if err != nil {
		return CommandResult{}, err
	}
	commandID := ""
	if c.options.Observer != nil || c.options.ControlOwnerExecutable != "" {
		commandID, err = randomid.NewRandomID()
		if err != nil {
			return CommandResult{}, err
		}
	}
	finishWork, err := c.beginCommandWork()
	if err != nil {
		return CommandResult{}, err
	}
	defer func() {
		if finishWork != nil {
			finishWork()
		}
	}()
	if consume != nil {
		if err := consume(); err != nil {
			return CommandResult{}, err
		}
	}
	statusVersion := ""
	if isJSONStatusCommand(args) {
		statusVersion = c.runtimeBinding.ObservedVersion
	}
	var pending *Pending
	if c.options.ControlOwnerExecutable == "" {
		pending = startOwnedCaptured(cmd, commandID, c.options.Observer, statusVersion)
	} else {
		pending = startControlOwnedCaptured(ctx, c.options.ControlOwnerExecutable, cmd, commandID, c.options.Observer, statusVersion)
	}
	finishPending := finishWork
	finishWork = nil
	go func() {
		<-pending.Done()
		finishPending()
	}()
	if statusVersion != "" {
		pending.SetStatusIdentity(c.runtimeBinding, statusVersion)
	}
	result, observeErr := observeCommand(ctx, pending, c.options.ObservationTimeout)
	if statusVersion == "" {
		return result, observeErr
	}
	return result, statusObservationError(observeErr)
}

func isJSONStatusCommand(args []string) bool {
	if len(args) < 2 || args[0] != "status" {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--json" {
			return true
		}
	}
	return false
}

// prepareCommand constructs the fully bound supervisor command without
// starting a process. It is kept separate so command construction can be
// inspected independently from observer setup, permit consumption, and
// process ownership.
func (c *Client) prepareCommand(args ...string) (*exec.Cmd, error) {
	environment := environmentForCommand(c.options.Environment)
	resolution, err := c.resolutionForEnvironment(environment)
	if err != nil {
		return nil, err
	}
	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "-c", c.runtimeBinding.ConfigPath)
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(c.runtimeBinding.ClientExecutable, commandArgs...)
	cmd.Dir = resolution.Cwd
	if cmd.Dir == "" {
		// Saved bindings intentionally omit the caller's working directory. Use
		// the existing config directory for recovery so a removed ambient cwd
		// cannot prevent a bound supervisor command from starting.
		cmd.Dir = filepath.Dir(c.runtimeBinding.ConfigPath)
	}
	cmd.Env = environment
	return cmd, nil
}
