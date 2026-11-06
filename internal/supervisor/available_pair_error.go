package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Only positive installation absence is optional. Selection and observation
// keep every independent context or filesystem fault.
func resolveCompatiblePrivatePair(ctx context.Context, root string, lookup ExecutableLookup) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	client, daemon, err := ResolvePrivateSupervisorExecutables(root, lookup)
	if unavailableBundledPair(err) && ctx.Err() == nil {
		return "", "", pueue.ErrCompatiblePairUnavailable
	}
	return client, daemon, errors.Join(err, ctx.Err())
}

// Only a raw installation lookup can classify this observation. Inventory
// validation returns before this boundary, so missing inventory members remain
// hard integrity errors. The original lookup error remains available to callers.
type unavailableBundledPairError struct{ cause error }

func (e *unavailableBundledPairError) Error() string { return e.cause.Error() }
func (e *unavailableBundledPairError) Unwrap() error { return e.cause }

func classifyUnavailableBundledPair(err error) error {
	if !missingBundledPairCauses(err) {
		return err
	}
	return &unavailableBundledPairError{cause: err}
}

func unavailableBundledPair(err error) bool {
	var unavailable *unavailableBundledPairError
	return errors.As(err, &unavailable) && unavailable != nil && missingBundledPairCauses(err)
}

func missingBundledPairCauses(err error) bool {
	return errors.Is(err, os.ErrNotExist) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, os.ErrNotExist) || errors.Is(cause, pueue.ErrConfiguration)
	})
}
