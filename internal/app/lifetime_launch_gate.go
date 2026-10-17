package app

import (
	"errors"
	"io"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
)

func runLifetimeLaunchGateMode(args []string, stderr io.Writer) (int, bool) {
	if len(args) == 0 || args[0] != guardian.LaunchGateMode {
		return 0, false
	}
	if err := guardian.RunLaunchGate(args[1:], syscall.Exec); err != nil {
		return writeCLIError(stderr, errors.New("task lifetime launch unavailable"), 1), true
	}
	return 0, true
}
