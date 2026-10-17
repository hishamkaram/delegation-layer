package supervisor

import (
	"errors"
)

// ErrExternalInspectionHandoffRequired retains the external-group recovery cause.
var ErrExternalInspectionHandoffRequired = errors.New("external inspection journal requires private routing handoff")
