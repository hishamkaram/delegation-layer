package control

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ProbeSavedPrivate observes a saved private generation without recovering,
// publishing, cleaning the queue, or starting a daemon. A ready client preserves
// the saved Binding while RuntimeBinding identifies its verified current
// successor. The caller owns the returned client and must close it. A nil client
// and nil error mean only not ready, never permission to recover or start.
func ProbeSavedPrivate(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, error) {
	_, cancel, probe, err := prepareSavedPrivateProbe(ctx, stateRoot, saved, clientExecutable, daemonExecutable, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return probe.client, nil
}

// savedPrivateProbe carries normalized options and the observed client. The
// separately returned finite context remains explicit: recovery retains it
// across its exclusive phase, while a warm-only observation cancels on return.
type savedPrivateProbe struct {
	options Options
	client  *Client
}

func prepareSavedPrivateProbe(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (context.Context, context.CancelFunc, savedPrivateProbe, error) {
	probeContext, cancel, options, err := prepareSavedPrivateProbeOptions(ctx, stateRoot, saved, options)
	if err != nil {
		return nil, nil, savedPrivateProbe{}, err
	}
	client, err := probeSavedPrivate(probeContext, stateRoot, saved, clientExecutable, daemonExecutable, options)
	if err != nil {
		cancel()
		return nil, nil, savedPrivateProbe{}, err
	}
	return probeContext, cancel, savedPrivateProbe{options: options, client: client}, nil
}

func prepareSavedPrivateProbeOptions(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options) (context.Context, context.CancelFunc, Options, error) {
	if !IsPrivateConfig(stateRoot, saved.ConfigPath) {
		return nil, nil, Options{}, fmt.Errorf("%w: supervisor config is not private to the state root", ErrConfiguration)
	}
	if err := validatePrivateSupervisorRef(saved); err != nil {
		return nil, nil, Options{}, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	probeContext, cancel, options, err := privateBootstrapOptions(ctx, options)
	if err != nil {
		return nil, nil, Options{}, err
	}
	options, err = optionsForSavedBinding(saved, options)
	if err != nil {
		cancel()
		return nil, nil, Options{}, err
	}
	// Validate the exact saved config before any endpoint or client probe. An
	// absent config must remain ErrBinding, not Bind's later ErrConfiguration.
	if err = validateSavedPrivateProbeConfig(stateRoot, saved, options); err != nil {
		cancel()
		return nil, nil, Options{}, err
	}
	return probeContext, cancel, options, nil
}
