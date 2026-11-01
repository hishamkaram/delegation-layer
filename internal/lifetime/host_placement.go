package lifetime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrHostContained = errors.New("scheduler host is inside a Delegate task scope")

// CheckIndependentHost rejects positive containment evidence. A nil result is
// not an attestation of historical independence: the user must launch the host
// from an independent terminal or tmux session. It never mutates a scope.
func CheckIndependentHost(ctx context.Context, environment []string) error {
	return checkIndependentHost(ctx, environment, checkNativeHostPlacement)
}

func checkIndependentHost(ctx context.Context, environment []string, native func(context.Context) error) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == processScopeMarkerEnvironment && validateProcessGroupPlanIdentifier(value) == nil {
			return hostContainedError()
		}
	}
	return native(ctx)
}

func hostContainedError() error {
	return fmt.Errorf("%w: run scheduler serve yourself from an independent terminal or tmux session", ErrHostContained)
}
