package control

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Pair verification is separate from endpoint readiness and generation phase.
// Only positively missing executable coordinates prove the Unverified state. Filesystem, config and cleanup faults are operational errors.
type privateProbePairVerification uint8

const (
	privateProbePairVerified privateProbePairVerification = iota + 1
	privateProbePairUnverified
	privateProbePairNotReady
)

func verifyPrivateProbePair(binding task.SupervisorRef) (privateProbePairVerification, error) {
	missing, err := PrivateSupervisorPairMissing(binding)
	if err != nil {
		return 0, err
	}
	verification := privateProbePairVerified
	if missing {
		verification = privateProbePairUnverified
	}
	for _, path := range []string{binding.ClientExecutable, binding.DaemonExecutable} {
		if path == "" {
			continue
		}
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			verification = privateProbePairUnverified
			continue
		} else if statErr != nil {
			return 0, statErr
		}
		canonical, canonicalErr := canonicalExecutable(path)
		if canonicalErr != nil {
			return 0, canonicalErr
		}
		if canonical != path {
			return 0, ErrBinding
		}

	}
	return verification, nil
}

func probeExactPrivatePair(ctx context.Context, published task.SupervisorRef, resolution ResolutionContext, options Options) (*Client, privateProbePairVerification, PrivateEndpointObservation, error) {
	verification, err := verifyPrivateProbePair(published)
	if err != nil || verification != privateProbePairVerified {
		return nil, verification, PrivateEndpointObservation{}, err
	}
	options.Resolution = &resolution
	client, err := NewClient(published, options)
	if err != nil {
		return nil, 0, PrivateEndpointObservation{}, err
	}
	if err = ctx.Err(); err != nil {
		return client, 0, PrivateEndpointObservation{}, err
	}
	comparison, err := client.compareBinding()
	if err != nil {
		return client, 0, PrivateEndpointObservation{}, errors.Join(err, ctx.Err())
	}
	if err = ctx.Err(); err != nil {
		return client, 0, PrivateEndpointObservation{}, err
	}
	if comparison != bindingEqual {
		return client, privateProbePairNotReady, PrivateEndpointObservation{}, nil
	}
	if err = attachPrivateDaemon(client, published.DaemonExecutable); err != nil {
		return client, 0, PrivateEndpointObservation{}, err
	}
	probed, _, observation, err := probeVerifiedPrivateClient(ctx, client, privateProbeSnapshot)
	return probed, verification, observation, err
}
