package tasklifecycle

import (
	"errors"
)

var errPredecessorOutcomeUnavailable = errors.New("predecessor has no authoritative terminal outcome")
