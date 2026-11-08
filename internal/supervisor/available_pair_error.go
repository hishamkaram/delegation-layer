package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
)

// Only positive installation absence is optional; independent faults remain errors.
func resolveCompatiblePrivatePair(ctx context.Context, lookup installation.ExecutableLookup) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	client, daemon, err := installation.ResolvePrivateSupervisorExecutables(lookup)
	if installation.UnavailableBundledPair(err) && ctx.Err() == nil {
		return "", "", pueue.ErrCompatiblePairUnavailable
	}
	return client, daemon, errors.Join(err, ctx.Err())
}
