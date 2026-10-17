package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func bindPrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, expected *task.SupervisorRef) (client *Client, resultErr error) {
	bootstrapContext, cancel, options, err := privateBootstrapOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return bindPrivateWithBootstrapLock(bootstrapContext, clientExecutable, daemonExecutable, stateRoot, options, expected)
}

func bindPrivateWithBootstrapLock(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, expected *task.SupervisorRef) (client *Client, resultErr error) {
	if err := WithBootstrapLockContext(ctx, stateRoot, func(lockedContext context.Context) error {
		client, resultErr = bindPrivateLocked(lockedContext, clientExecutable, daemonExecutable, stateRoot, options, expected)
		return resultErr
	}); err != nil {
		resultErr = err
	}
	return client, resultErr
}

func bindPrivateLocked(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, expected *task.SupervisorRef) (*Client, error) {
	resolution, err := privateResolution(options)
	if err != nil {
		return nil, err
	}
	clientPath, err := canonicalExecutable(clientExecutable)
	if err != nil {
		return nil, err
	}
	daemonPath, err := canonicalExecutable(daemonExecutable)
	if err != nil {
		return nil, err
	}
	return bindPrivateResolved(ctx, clientPath, daemonPath, stateRoot, resolution, options, expected)
}

func bindPrivateResolved(ctx context.Context, clientPath, daemonPath, stateRoot string, resolution ResolutionContext, options Options, expected *task.SupervisorRef) (*Client, error) {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return nil, err
	}
	if _, err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return nil, err
	}
	configPath, base, err := privateBindingConfig(stateRoot, base, resolution, expected)
	if err != nil {
		return nil, err
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, expected); err != nil {
		return nil, err
	}
	client, ready, err := bindPrivateClient(ctx, clientPath, daemonPath, configPath, options)
	if err != nil {
		return nil, err
	}
	if err = matchExpectedPrivateBinding(client, expected); err != nil {
		return nil, err
	}
	if err = restoreExpectedPrivateSupervisorBinding(ctx, stateRoot, expected); err != nil {
		return nil, err
	}
	if err = verifyPrivateSupervisorBinding(stateRoot, client.Binding(), ready); err != nil {
		return nil, err
	}
	if ready {
		return client, nil
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, expected); err != nil {
		return nil, err
	}
	if err = startDaemon(daemonPath, client.Binding().DaemonSHA256, configPath, base, options); err != nil {
		return nil, err
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		return nil, fmt.Errorf("%w: private pueued did not become ready: %w", ErrBinding, err)
	}
	if err = publishPrivateDaemonIdentity(base, client.Binding()); err != nil {
		return nil, err
	}
	if err = clearPrivateBootstrapPending(stateRoot); err != nil {
		return nil, err
	}
	return client, nil
}

func restoreExpectedPrivateSupervisorBinding(ctx context.Context, stateRoot string, expected *task.SupervisorRef) error {
	if expected == nil {
		return nil
	}
	return restorePrivateSupervisorBindingFromSaved(ctx, stateRoot, *expected)
}

// SetPrivateDefaultParallelTasksWithBootstrapLock applies one generic Pueue
// default-group parallelism value while the caller owns WithBootstrapLock. It
// returns the bounded post-change group observation and stores no application
// policy evidence.
func (c *Client) SetPrivateDefaultParallelTasksWithBootstrapLock(ctx context.Context, stateRoot string, parallelTasks uint64) (map[string]Group, error) {
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return nil, ErrConfiguration
	}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if _, _, err = c.privateGroups(controlContext); err != nil {
		return nil, errors.Join(ErrBinding, err)
	}
	if err = c.setPrivateDefaultParallelTasksLocked(controlContext, parallelTasks); err != nil {
		return nil, err
	}
	groups, _, err := c.privateGroups(controlContext)
	if err != nil {
		return nil, errors.Join(ErrBinding, err)
	}
	group, found := groups["default"]
	if !found || group.ParallelTasks != parallelTasks {
		return nil, fmt.Errorf("%w: private supervisor did not retain default-group parallelism", ErrBinding)
	}
	return groups, nil
}

func (c *Client) setPrivateDefaultParallelTasksLocked(ctx context.Context, parallelTasks uint64) error {
	if err := c.verifyForPrivateGroupConfiguration(ctx); err != nil {
		return err
	}
	result, err := c.command(ctx, nil, "parallel", strconv.FormatUint(parallelTasks, 10))
	if err != nil {
		if pending := pendingFrom(err); pending != nil {
			settled, waitErr := c.awaitPendingForControl(ctx, pending)
			if waitErr == nil && settled.Err == nil {
				return nil
			}
			result = settled
			err = errors.Join(err, waitErr)
		}
		return fmt.Errorf("%w: configure private default-group parallelism: %w", ErrBinding, errors.Join(err, result.Err))
	}
	return nil
}

func (c *Client) verifyForPrivateGroupConfiguration(ctx context.Context) error {
	if err := c.verify(ctx); err != nil {
		return fmt.Errorf("%w: verify supervisor before configuring default-group parallelism: %w", ErrBinding, err)
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return ErrConfiguration
	}
	return c.authorizePrivateMutationWithLock(stateRoot)
}

func (c *Client) awaitPendingForControl(ctx context.Context, pending *Pending) (CommandResult, error) {
	if ctx == nil {
		return CommandResult{}, context.Canceled
	}
	timeout := c.options.ObservationTimeout
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return awaitPending(waitContext, pending)
}

// settlePendingControlObservation gives an exact task control one bounded
// chance to finish a read-only observation before it decides whether to mutate
// its saved numeric target. Callers must pass only reconciliation functions:
// a mutation that becomes pending is never retried here.
func settlePendingControlObservation[T any](c *Client, ctx context.Context, observe func(context.Context) (T, error)) (T, error) {
	var zero T
	if c == nil || observe == nil {
		return zero, ErrConfiguration
	}
	observation, err := observe(ctx)
	pending := pendingFrom(err)
	if pending == nil {
		return observation, err
	}
	if _, waitErr := c.awaitPendingForControl(ctx, pending); waitErr != nil {
		return observation, errors.Join(ErrUnknown, waitErr)
	}
	return observe(ctx)
}

// PrivateControlTimeout bounds the complete private supervisor control
// sequence, including migration. The migration path may bind the current
// pair, reopen the legacy endpoint, inspect its queue, stop it, wait for the
// endpoint to disappear, restart the successor, and verify readiness. The
// observation budget therefore covers those phases as well as ordinary
// bootstrap and reconciliation.
func PrivateControlTimeout(options Options) time.Duration {
	observationTimeout := options.ObservationTimeout
	if observationTimeout <= 0 {
		observationTimeout = DefaultObservationTimeout
	}
	const maxDuration = time.Duration(1<<63 - 1)
	const phases = privateBootstrapObservationPhases + privateReconciliationObservationPhases + privateMigrationObservationPhases
	if observationTimeout > maxDuration/phases {
		return maxDuration
	}
	return observationTimeout * phases
}

func privateBootstrapOptions(ctx context.Context, options Options) (context.Context, context.CancelFunc, Options, error) {
	if ctx == nil {
		return nil, nil, Options{}, context.Canceled
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, nil, Options{}, err
	}
	bootstrapContext, cancel, err := privateBootstrapContext(ctx, normalized.ObservationTimeout)
	if err != nil {
		return nil, nil, Options{}, err
	}
	return bootstrapContext, cancel, normalized, nil
}

// privateProbeOptions preserves an explicit caller control deadline. Readiness
// commands remain individually bounded by Options.ObservationTimeout, while a
// caller that owns a full private-supervisor transition does not lose most of
// its budget merely by observing the generation before taking that transition.
func privateProbeOptions(ctx context.Context, options Options) (context.Context, context.CancelFunc, Options, error) {
	if ctx == nil {
		return nil, nil, Options{}, context.Canceled
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, nil, Options{}, err
	}
	if _, bounded := ctx.Deadline(); bounded {
		return ctx, func() {}, normalized, nil
	}
	probeContext, cancel, err := privateBootstrapContext(ctx, normalized.ObservationTimeout)
	if err != nil {
		return nil, nil, Options{}, err
	}
	return probeContext, cancel, normalized, nil
}

func privateBootstrapContext(ctx context.Context, observationTimeout time.Duration) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if observationTimeout <= 0 || observationTimeout > maxDuration/privateBootstrapObservationPhases {
		return nil, nil, ErrConfiguration
	}
	bootstrapContext, cancel := context.WithTimeout(ctx, observationTimeout*privateBootstrapObservationPhases)
	return bootstrapContext, cancel, nil
}

func preparePrivateBootstrap(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (normalized Options, resolution ResolutionContext, clientPath, daemonPath, base string, lock *bootstrapLock, resultErr error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return Options{}, ResolutionContext{}, "", "", "", nil, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	resolution, err = privateResolution(normalized)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	clientPath, err = canonicalExecutable(clientExecutable)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	daemonPath, err = canonicalExecutable(daemonExecutable)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	base = filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = ensurePrivateBase(base); err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	lock, err = acquireBootstrapLock(ctx, filepath.Join(base, "bootstrap.lock"))
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	return normalized, resolution, clientPath, daemonPath, base, lock, nil
}

func privateBindingConfig(stateRoot, base string, resolution ResolutionContext, expected *task.SupervisorRef) (configPath, privateBase string, resultErr error) {
	if expected == nil {
		return ensurePrivateConfig(stateRoot, resolution)
	}
	if !IsPrivateConfig(stateRoot, expected.ConfigPath) {
		return "", "", fmt.Errorf("%w: saved supervisor config is not private to the state root", ErrConfiguration)
	}
	// Recovery must fail closed when the saved config is absent or changed; it
	// must never recreate a default config before checking the saved binding,
	// because that could start a different queue.
	configPath = expected.ConfigPath
	if err := rejectSymlinkComponents(configPath); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(configPath); err != nil {
		return "", "", fmt.Errorf("%w: saved private supervisor config is unavailable: %w", ErrBinding, err)
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func verifyExpectedPrivateBinding(ctx context.Context, clientPath, daemonPath, configPath string, options Options, expected *task.SupervisorRef) error {
	if expected == nil {
		return nil
	}
	bound, err := Bind(ctx, clientPath, configPath, options)
	if err != nil {
		return err
	}
	if err := attachPrivateDaemon(bound, daemonPath); err != nil {
		return err
	}
	return matchExpectedPrivateBinding(bound, expected)
}

func matchExpectedPrivateBinding(client *Client, expected *task.SupervisorRef) error {
	if expected != nil && !privateBindingsMatch(client.Binding(), *expected) {
		return ErrBinding
	}
	return nil
}

func privateBindingsMatch(actual, expected task.SupervisorRef) bool {
	// Executable and resolver fields describe the client and daemon selected by
	// a particular installation. Durable private ownership is instead bound to
	// the state-rooted config and endpoint; live compatibility is checked by
	// the Pueue readiness/status handshake.
	return task.SameSupervisorIdentity(actual, expected)
}

// privateFullGenerationMatches compares every persisted field of a supervisor
// generation. Logical identity is intentionally weaker and is used only for
// live adoption after the current generation or migration history has proved
// that the saved task belongs to it.
func privateFullGenerationMatches(left, right task.SupervisorRef) bool {
	return left == right
}

func probePrivateCompatiblePair(ctx context.Context, saved, expected task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, bool, error) {
	client, ready, err := bindPrivateClientWithCleanup(ctx, clientExecutable, daemonExecutable, expected.ConfigPath, options, false)
	if err != nil {
		return nil, false, err
	}
	if !privateBindingsMatch(client.Binding(), expected) {
		return nil, false, errors.Join(ErrBinding, client.Close())
	}
	if !ready {
		return nil, false, client.Close()
	}
	// A compatible pair is a runtime client only. The completed generation is
	// the compatibility authority; the saved task reference remains the
	// durable authority while the endpoint is live.
	client.binding = saved
	return client, true, nil
}
