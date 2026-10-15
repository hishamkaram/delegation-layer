//go:build linux || darwin

package lifetime

import (
	"fmt"
	"os/exec"
	"syscall"
)

// ConfigureIndependentSession isolates an infrastructure process from the
// caller's terminal, process group, and session. The caller retains ownership
// of starting and waiting for the command.
func ConfigureIndependentSession(cmd *exec.Cmd) error {
	if cmd == nil {
		return fmt.Errorf("%w: nil command", ErrInvalidRef)
	}
	if cmd.Process != nil || cmd.ProcessState != nil {
		return fmt.Errorf("%w: command has already started", ErrConflictingSysProc)
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
		if attr.Setctty || attr.Foreground || attr.Ctty != 0 || attr.Pgid != 0 || attr.Setpgid {
			return fmt.Errorf("%w: independent session conflicts with terminal or PGID settings", ErrConflictingSysProc)
		}
		if attr.Setsid {
			return nil
		}
	}
	attr.Setsid = true
	cmd.SysProcAttr = &attr
	return nil
}
