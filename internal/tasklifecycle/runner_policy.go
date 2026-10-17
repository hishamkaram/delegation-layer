package tasklifecycle

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func RunnerStartStateError(state pueue.State) error {
	switch state {
	case pueue.StateQueued, pueue.StateStashed, pueue.StateRunning:
		return nil
	case pueue.StateEnded:
		return errors.New("supervisor task ended before runner start")
	case pueue.StateUnknown:
		return fmt.Errorf("%w: supervisor task state unknown before runner start", pueue.ErrUnknown)
	default:
		return fmt.Errorf("%w: unsupported supervisor task state %q before runner start", pueue.ErrUnknown, state)
	}
}
