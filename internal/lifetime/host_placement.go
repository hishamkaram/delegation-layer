package lifetime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrHostContained = errors.New("scheduler host is inside a Delegate task scope")
	// ErrTaskContained marks positive evidence of current Delegate task containment.
	ErrTaskContained = errors.New("current process is inside a Delegate task scope")
)

// CheckIndependentHost rejects positive containment evidence. A nil result is
// not an attestation of historical independence: the user must launch the host
// from an independent terminal or tmux session. It never mutates a scope.
func CheckIndependentHost(ctx context.Context, environment []string) error {
	return checkIndependentHost(ctx, environment, checkNativeHostPlacement)
}

// CheckCurrentTaskContainment rejects positive evidence from the captured
// environment or native task scope. Nil means only that no positive evidence
// was observed; it never certifies outside placement or historical independence.
// The lifetime subsystem owns these observations and never mutates a scope.
func CheckCurrentTaskContainment(ctx context.Context, environment []string) error {
	return checkCurrentTaskContainment(ctx, environment, checkNativeHostPlacement)
}

func checkIndependentHost(ctx context.Context, environment []string, native func(context.Context) error) error {
	err := checkCurrentTaskContainment(ctx, environment, native)
	if errors.Is(err, ErrTaskContained) {
		return hostContainedError()
	}
	return err
}

func checkCurrentTaskContainment(ctx context.Context, environment []string, native func(context.Context) error) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == processScopeMarkerEnvironment && validateProcessGroupPlanIdentifier(value) == nil {
			return ErrTaskContained
		}
	}
	return native(ctx)
}

func hostContainedError() error {
	return fmt.Errorf("%w: run scheduler serve yourself from an independent terminal or tmux session", ErrHostContained)
}
