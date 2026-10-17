package supervisor

import (
	"errors"
)

var errStateTerminalEventRunnerRevisionDeferred = errors.New("terminal event runner publication deferred to a newer implementation")
