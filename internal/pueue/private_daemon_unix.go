//go:build linux || darwin

package pueue

import (
	"fmt"
	"os/exec"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// configurePrivateDaemonCommand gives the private daemon the descriptor limit
// already available to Delegate and isolates it from the caller's session.
// Calling syscall.Setrlimit with the current values also tells Go not to
// restore its pre-runtime RLIMIT_NOFILE in this child.
func configurePrivateDaemonCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.SysProcAttr != nil {
		return ErrConfiguration
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("read descriptor limit: %w", err)
	}
	if limit.Cur == 0 || limit.Max == 0 || limit.Cur > limit.Max {
		return fmt.Errorf("%w: invalid descriptor limit", ErrConfiguration)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return fmt.Errorf("preserve descriptor limit: %w", err)
	}
	if err := lifetime.ConfigureIndependentSession(cmd); err != nil {
		return fmt.Errorf("%w: configure private daemon session: %w", ErrConfiguration, err)
	}
	return nil
}
