package tasklifecycle

import (
	"errors"
)

var errPredecessorOutcomeUnavailable = errors.New("predecessor has no authoritative terminal outcome")

// errPredecessorSessionAbsent marks a predecessor with neither a recorded
// provider identity nor its own requested prior session.
var errPredecessorSessionAbsent = errors.New("predecessor session is unavailable")
