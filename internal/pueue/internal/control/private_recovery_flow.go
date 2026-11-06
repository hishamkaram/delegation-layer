package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrivateRecovery is the result of one private recovery attempt. It holds
// either the recovered client or the verified fact that the completed
// published pair is missing and no compatible pair answers. The fact names no
// action; the supervisor owns the decision to reconcile.
type PrivateRecovery struct {
	client          *Client
	cause           error
	pairUnavailable bool
}

// Client returns the recovered client, or nil when the pair is unavailable.
func (r PrivateRecovery) Client() *Client { return r.client }

// PairUnavailable reports the verified missing-pair fact.
func (r PrivateRecovery) PairUnavailable() bool { return r.pairUnavailable }

// Cause returns the diagnostic error behind PairUnavailable, or nil.
func (r PrivateRecovery) Cause() error { return r.cause }

// AttemptPrivateRecovery reopens a saved private task through the state-rooted
// supervisor. It first adopts a live, completed successor proven to own the
// saved generation; a stopped endpoint is restarted only from that exact
// completed publication. A genuinely legacy root may restart only the exact
// saved pair when its daemon identity proves the same generation. The durable
// task binding remains unchanged while the runtime binding names the active
// generation actually used for control. Every operational fault is returned
// as the error, never as PairUnavailable.
func AttemptPrivateRecovery(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (PrivateRecovery, error) {
	bootstrapContext, cancel, probe, err := prepareSavedPrivateProbe(ctx, stateRoot, saved, clientExecutable, daemonExecutable, options)
	if err != nil {
		return PrivateRecovery{}, err
	}
	defer cancel()
	if probe.client != nil {
		return PrivateRecovery{client: probe.client}, nil
	}
	options = probe.options
	var recovery PrivateRecovery
	if err = WithBootstrapLockContext(bootstrapContext, stateRoot, func(lockedContext context.Context) error {
		if validationErr := validateSavedPrivateConfig(lockedContext, stateRoot, saved, options); validationErr != nil {
			return validationErr
		}
		// A pending explicit repair may have preserved the old config under a
		// root-owned alias. Reopen that exact endpoint first so its queued work
		// remains recoverable; this is the only recovery exception to the
		// current state-root config.
		recovered, _, found, recoveryErr := recoverPrivateRecoveryBinding(lockedContext, stateRoot, saved, clientExecutable, daemonExecutable, options)
		if recoveryErr != nil || found {
			recovery = PrivateRecovery{client: recovered}
			return recoveryErr
		}
		recovery, recoveryErr = recoverPrivateGenerationWithLock(lockedContext, stateRoot, saved, clientExecutable, daemonExecutable, options)
		return recoveryErr
	}); err != nil {
		return PrivateRecovery{}, errors.Join(err, recovery.client.CloseAfterPending(err))
	}
	return recovery, nil
}

func validateSavedPrivateConfig(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options) error {
	resolution, err := privateResolution(options)
	if err != nil {
		return err
	}
	_, _, err = privateBindingConfig(ctx, stateRoot, filepath.Join(stateRoot, privateSupervisorDirectory), resolution, &saved, options)
	return err
}

func probeSavedPrivate(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, error) {
	probed, ready, _, err := observePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeSnapshot, privateGenerationObservable, func(checkContext context.Context, client *Client, _ QueueSnapshot) (bool, error) {
		current, found, resolveErr := ResolvePrivateSupervisorSuccessorWithLock(checkContext, stateRoot, saved)
		if resolveErr != nil {
			return false, resolveErr
		}
		return found && privateFullGenerationMatches(client.Binding(), current), nil
	})
	return finishSavedPrivateProbe(saved, probed, ready, err)
}

func finishSavedPrivateProbe(saved task.SupervisorRef, probed *Client, ready bool, err error) (*Client, error) {
	if err != nil {
		return nil, bindingProbeError(err)
	}
	if ready {
		probed.binding = saved
		return probed, nil
	}
	// An absent or refused endpoint is not ready. Exclusive recovery
	// re-observes it before its canonical closure; a close fault still fails.
	if probed != nil {
		return nil, probed.Close()
	}
	return nil, nil
}

func recoverPrivateGenerationWithLock(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (PrivateRecovery, error) {
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return PrivateRecovery{}, fmt.Errorf("%w: read completed private supervisor generation: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !found {
		client, legacyErr := recoverLegacyPrivateGeneration(ctx, stateRoot, saved, options)
		return PrivateRecovery{client: client}, legacyErr
	}

	current, connected, err := ResolvePrivateSupervisorSuccessorWithLock(ctx, stateRoot, saved)
	if err != nil {
		return PrivateRecovery{}, err
	}
	if !connected || !privateFullGenerationMatches(current, published) {
		return PrivateRecovery{}, fmt.Errorf("%w: saved private supervisor is unrelated to the completed active generation", ErrPrivateSupervisorMigrationRequired)
	}

	client, prepared, pairErr, prepareErr := preparePublishedPrivate(ctx, stateRoot, options)
	if prepareErr == nil && prepared && pairErr == nil {
		if err = client.StartPrivate(ctx, stateRoot); err != nil {
			return PrivateRecovery{}, errors.Join(err, client.CloseAfterPending(err))
		}
		client.binding = saved
		return PrivateRecovery{client: client}, nil
	}
	return recoverUnpreparedPublishedPrivate(ctx, unpreparedPublishedPrivate{
		stateRoot:        stateRoot,
		saved:            saved,
		published:        published,
		clientExecutable: clientExecutable,
		daemonExecutable: daemonExecutable,
		options:          options,
		prepared:         prepared,
		pairErr:          pairErr,
		prepareErr:       prepareErr,
	})
}

// unpreparedPublishedPrivate is the completed generation whose published pair
// could not be prepared for a clean start, with the preparation outcome.
type unpreparedPublishedPrivate struct {
	stateRoot        string
	saved            task.SupervisorRef
	published        task.SupervisorRef
	clientExecutable string
	daemonExecutable string
	options          Options
	prepared         bool
	pairErr          error
	prepareErr       error
}

func recoverUnpreparedPublishedPrivate(ctx context.Context, attempt unpreparedPublishedPrivate) (PrivateRecovery, error) {
	diagnostic := errors.Join(attempt.prepareErr, privatePublishedRecoveryError(attempt.pairErr))
	if complete, completeErr := privateSupervisorMigrationComplete(attempt.stateRoot, attempt.published, false); completeErr != nil {
		return PrivateRecovery{}, errors.Join(completeErr, diagnostic)
	} else if !complete || (!attempt.prepared && attempt.prepareErr == nil) {
		return PrivateRecovery{}, errors.Join(
			fmt.Errorf("%w: completed private supervisor transition changed during recovery", ErrPrivateSupervisorMigrationRequired),
			diagnostic,
		)
	}
	// A completed migration is not permission to discard a preparation or
	// close fault; only a verification failure of the published pair may be
	// inspected through a compatible pair.
	if attempt.prepareErr != nil {
		return PrivateRecovery{}, diagnostic
	}
	missing, err := PrivateSupervisorPairMissing(attempt.published)
	if err != nil {
		return PrivateRecovery{}, errors.Join(diagnostic, err)
	}
	// A compatible pair can still inspect the live endpoint of the completed
	// generation, but it is never allowed to start a stopped generation or
	// publish its own identity. Its own fault is the operational result.
	compatible, ready, probeErr := probePrivateCompatiblePair(ctx, attempt.saved, attempt.published, attempt.clientExecutable, attempt.daemonExecutable, attempt.options)
	if probeErr != nil {
		return PrivateRecovery{}, probeErr
	}
	if ready {
		return PrivateRecovery{client: compatible}, nil
	}
	cause := errors.Join(
		fmt.Errorf("%w: completed private supervisor pair is unavailable for restart", ErrPrivateSupervisorMigrationRequired),
		diagnostic,
	)
	if !missing {
		// A present pair that fails verification is not the missing-pair fact.
		return PrivateRecovery{}, cause
	}
	return PrivateRecovery{pairUnavailable: true, cause: cause}, nil
}

func recoverLegacyPrivateGeneration(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options) (*Client, error) {
	if pending, err := privateRecoveryTransitionPending(stateRoot); err != nil {
		return nil, err
	} else if pending {
		return nil, fmt.Errorf("%w: private supervisor transition must be completed before legacy recovery", ErrPrivateSupervisorMigrationRequired)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	identity, found, err := loadPrivateDaemonIdentity(base)
	if err != nil {
		return nil, fmt.Errorf("%w: read legacy private supervisor identity: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !found || saved.DaemonExecutable == "" || !privateDaemonIdentityMatchesBinding(identity, saved) {
		return nil, fmt.Errorf("%w: legacy private supervisor generation cannot be proven from saved identity", ErrPrivateSupervisorMigrationRequired)
	}

	client, err := BindSaved(ctx, saved, options)
	if err != nil {
		return nil, errors.Join(err, client.CloseAfterPending(err))
	}
	observation, err := client.availableForTransition(ctx, false)
	if err != nil {
		return nil, errors.Join(err, client.CloseAfterPending(err))
	}
	if !observation.ready() {
		if launchErr := client.validatePrivateGenerationLaunch(stateRoot); launchErr != nil {
			return nil, errors.Join(launchErr, client.CloseAfterPending(launchErr))
		}
		if err = client.closeRefusedPrivateEndpoint(ctx, observation); err == nil {
			err = client.startPrivate(ctx, stateRoot)
		}
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: restart exact legacy private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err),
				client.CloseAfterPending(err),
			)
		}
	}
	if err = client.EnsurePrivateSupervisorBindingWithLock(ctx, stateRoot); err != nil {
		return nil, errors.Join(err, client.CloseAfterPending(err))
	}
	client.binding = saved
	return client, nil
}

func privateRecoveryTransitionPending(stateRoot string) (bool, error) {
	migration, err := PrivateSupervisorMigrationPending(stateRoot)
	if err != nil || migration {
		return migration, err
	}
	bootstrap, err := PrivateBootstrapPending(stateRoot)
	if err != nil || bootstrap {
		return bootstrap, err
	}
	_, publication, err := readPrivateIdentityPublication(stateRoot)
	return publication, err
}

// RecoverSaved binds a legacy supervisor and, when its endpoint is absent,
// starts the exact recorded daemon pair long enough to inspect and migrate its
// queue. The returned bool is true only when this call started that daemon;
// callers may shut down only that recovery-owned endpoint.
func RecoverSaved(ctx context.Context, operationRoot string, saved task.SupervisorRef, options Options) (*Client, bool, error) {
	return RecoverSavedWithPair(ctx, operationRoot, saved, "", "", options)
}

// RecoverSavedWithPair is the migration form of RecoverSaved. It uses the
// saved client and daemon while those installation paths still exist, then
// falls back to a caller-supplied verified pair when an upgrade removed them.
// The durable binding remains the saved supervisor; the pair only supplies the
// executable implementation used to inspect or restart that same endpoint.
func RecoverSavedWithPair(ctx context.Context, operationRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, bool, error) {
	if err := validateSavedSupervisorRef(saved); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	_, private := privateStateRoot(saved.ConfigPath)
	stateRoot := operationRoot
	if private && !IsPrivateConfig(operationRoot, saved.ConfigPath) {
		return nil, false, ErrConfiguration
	}
	if private {
		resolvedClient, resolveErr := selectRecoveryClient(saved, clientExecutable)
		if resolveErr != nil {
			return nil, false, resolveErr
		}
		resolvedDaemon, resolveErr := selectRecoveryDaemon(saved, daemonExecutable)
		if resolveErr != nil {
			return nil, false, resolveErr
		}
		if recovered, started, found, recoveryErr := recoverPrivateRecoveryBinding(ctx, stateRoot, saved, resolvedClient, resolvedDaemon, options); found || recoveryErr != nil {
			if recoveryErr != nil {
				return nil, false, recoveryErr
			}
			return recovered, started, nil
		}
		clientExecutable = resolvedClient
	}
	return recoverSavedWithSelectedPair(ctx, saved, clientExecutable, daemonExecutable, options, stateRoot, private)
}

func recoverSavedWithSelectedPair(ctx context.Context, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options, stateRoot string, private bool) (*Client, bool, error) {
	options, err := optionsForSavedBinding(saved, options)
	if err != nil {
		return nil, false, err
	}
	clientExecutable, err = selectRecoveryClient(saved, clientExecutable)
	if err != nil {
		return nil, false, err
	}
	client, err := bindSavedForMigrationWithClient(ctx, saved, clientExecutable, options)
	if err != nil {
		return nil, false, errors.Join(err, client.CloseAfterPending(err))
	}
	if private {
		// Keep the verified predecessor daemon identity on the recovered client
		// even when its endpoint is already reachable. Migration must prove that
		// the process it shuts down is the saved supervisor before waiting for
		// the endpoint and process to disappear.
		daemon, daemonErr := selectRecoveryDaemon(saved, daemonExecutable)
		if daemonErr != nil {
			return nil, false, errors.Join(daemonErr, client.Close())
		}
		client.runtimeBinding.DaemonExecutable = daemon
		client.runtimeBinding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
		client.runtimeBinding.ClientSHA256, client.runtimeBinding.DaemonSHA256 = "", ""
	}
	observation, err := client.observeSavedRecoveryEndpoint(ctx, private)
	if err != nil {
		return nil, false, errors.Join(err, client.CloseAfterPending(err))
	}
	if observation.ready() {
		return client, false, nil
	}

	if private {
		if launchErr := client.validatePrivateGenerationLaunch(stateRoot); launchErr != nil {
			return nil, false, errors.Join(launchErr, client.CloseAfterPending(launchErr))
		}
	}
	daemon, err := selectRecoveryDaemon(saved, daemonExecutable)
	if err != nil {
		return nil, false, failPrivateTransitionClient(client, observation, err)
	}
	client.runtimeBinding.DaemonExecutable = daemon
	client.runtimeBinding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	client.runtimeBinding.ClientSHA256, client.runtimeBinding.DaemonSHA256 = "", ""
	if err = client.closeRefusedPrivateEndpoint(ctx, observation); err != nil {
		return nil, false, errors.Join(err, client.CloseAfterPending(err))
	}
	if err = startDaemon(ctx, stateRoot, daemon, client.runtimeBinding.ConfigPath, filepath.Dir(client.runtimeBinding.ConfigPath), options); err != nil {
		return nil, false, errors.Join(err, client.Close())
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		// Readiness failure is inconclusive: no successful private liveness
		// observation has proved that the recovered endpoint is safe to stop. Keep it alive and
		// leave a durable private recovery alias for the next bootstrap.
		combinedErr := err
		if private {
			aliasErr := client.PersistPrivateRecoveryBindingContext(ctx, stateRoot)
			combinedErr = errors.Join(combinedErr, aliasErr)
		}
		return nil, true, errors.Join(combinedErr, client.CloseAfterPending(combinedErr))
	}
	return client, true, nil
}

func (c *Client) observeSavedRecoveryEndpoint(ctx context.Context, private bool) (PrivateEndpointObservation, error) {
	observation, err := c.availableForTransition(ctx, true)
	if err == nil && !private {
		// External supervisors keep their original refused-endpoint failure.
		err = observation.projectedError()
	}
	return observation, err
}

var errSavedSupervisorExecutableUnavailable = errors.New("saved supervisor executable is unavailable")

func selectRecoveryExecutable(saved, fallback string) (string, error) {
	if saved != "" {
		if _, err := os.Stat(saved); err == nil {
			return saved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: inspect saved supervisor executable: %w", ErrBinding, err)
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("%w: %w", ErrBinding, errSavedSupervisorExecutableUnavailable)
	}
	return fallback, nil
}

func selectRecoveryClient(saved task.SupervisorRef, fallback string) (string, error) {
	if fallback != "" && isLegacySupervisorClientPath(filepath.Dir(filepath.Dir(saved.ConfigPath)), fallback) {
		return "", fmt.Errorf("%w: historical executable copy is not installed transport", ErrBinding)
	}
	if isLegacyStateSupervisorClient(saved) {
		if fallback == "" {
			return "", fmt.Errorf("%w: historical supervisor copy requires an installed client", ErrBinding)
		}
		return canonicalExecutable(fallback)
	}
	return selectRecoveryExecutable(saved.ClientExecutable, fallback)
}

func selectRecoveryDaemon(saved task.SupervisorRef, fallback string) (string, error) {
	if fallback != "" && isLegacySupervisorClientPath(filepath.Dir(filepath.Dir(saved.ConfigPath)), filepath.Join(filepath.Dir(fallback), "pueue")) {
		return "", fmt.Errorf("%w: historical executable copy is not installed transport", ErrBinding)
	}
	if !isLegacyStateSupervisorClient(saved) && saved.DaemonExecutable != "" {
		if _, err := os.Stat(saved.DaemonExecutable); err == nil {
			return savedDaemon(saved)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: inspect saved supervisor daemon: %w", ErrBinding, err)
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("%w: installed supervisor daemon is unavailable", ErrBinding)
	}
	resolved, err := canonicalExecutable(fallback)
	if err != nil {
		return "", fmt.Errorf("%w: installed supervisor daemon is unavailable: %w", ErrBinding, err)
	}
	return resolved, nil
}

// StopRecovered gracefully stops a supervisor that the caller started through
// RecoverSaved and waits until its endpoint is unavailable. It exists so a
// migration that fails while inspecting a recovery-owned queue can release
// that temporary daemon without duplicating the shutdown protocol.
func (c *Client) StopRecovered(ctx context.Context, timeout time.Duration, prior ...error) error {
	return stopRecoveredSupervisor(ctx, c, timeout, prior...)
}

func bindSavedMigrationClient(ctx context.Context, saved task.SupervisorRef, current *Client) (*Client, error) {
	if current == nil {
		return nil, ErrConfiguration
	}
	runtimeBinding := current.Binding()
	if runtimeBinding.ConfigPath != saved.ConfigPath {
		return nil, ErrBinding
	}
	if task.SameSupervisorIdentity(runtimeBinding, saved) {
		current.binding = saved
		return current, nil
	}
	if runtimeBinding.Endpoint == saved.Endpoint {
		if err := validateSameEndpointMigrationConfig(current, saved); err != nil {
			return nil, err
		}
		current.binding = saved
		current.runtimeBinding = runtimeBinding
		return current, nil
	}
	return bindSavedEndpointMigration(ctx, saved, current)
}

func validateSameEndpointMigrationConfig(current *Client, saved task.SupervisorRef) error {
	stateRoot, private := privateStateRoot(saved.ConfigPath)
	if !private {
		return nil
	}
	resolution, err := current.resolution()
	if err != nil {
		return err
	}
	data, err := readRegular(saved.ConfigPath, MaxControlBytes)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return err
	}
	return validateResolvedPrivateMigrationConfig(resolved, filepath.Join(stateRoot, privateSupervisorDirectory), saved.Endpoint)
}

func bindSavedEndpointMigration(ctx context.Context, saved task.SupervisorRef, current *Client) (*Client, error) {
	stateRoot, private := privateStateRoot(saved.ConfigPath)
	if !private {
		return nil, ErrBinding
	}
	data, err := readRegular(saved.ConfigPath, MaxControlBytes)
	if err != nil {
		return nil, errors.Join(ErrBinding, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return nil, err
	}
	resolution, err := current.resolution()
	if err != nil {
		return nil, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = validateResolvedPrivateMigrationConfig(resolved, base, ""); err != nil {
		return nil, err
	}
	socketPath, err := savedSocketPath(saved.Endpoint)
	if err != nil {
		return nil, err
	}
	if err = pathWithin(base, socketPath); err != nil {
		return nil, fmt.Errorf("%w: legacy private supervisor endpoint is outside its private directory", ErrBinding)
	}
	migrationData, err := configWithSocketPath(data, socketPath)
	if err != nil {
		return nil, err
	}
	migrationPath, cleanup, err := createMigrationConfig(base, migrationData)
	if err != nil {
		return nil, err
	}
	migration, err := Bind(ctx, current.runtimeBinding.ClientExecutable, migrationPath, current.options)
	if err != nil {
		if migration != nil {
			migration.cleanup = cleanup
			return nil, errors.Join(err, migration.CloseAfterPending(err))
		}
		return nil, errors.Join(err, retainCleanupUntil(cleanup, err))
	}
	if migration.Binding().Endpoint != saved.Endpoint {
		return nil, errors.Join(ErrBinding, cleanup())
	}
	migration.binding = saved
	migration.cleanup = cleanup
	if constructionErr := recordPrivateClientConstruction(migration, stateRoot); constructionErr != nil {
		return nil, errors.Join(constructionErr, migration.CloseAfterPending(constructionErr))
	}
	return migration, nil
}

func validateResolvedPrivateMigrationConfig(resolved ResolvedConfig, base, endpoint string) error {
	if err := resolved.ValidateIsolation(base); err != nil {
		return err
	}
	want := map[string]string{
		"pueue data":  filepath.Join(base, "data"),
		"runtime":     filepath.Join(base, "run"),
		"alias":       filepath.Join(base, "aliases.yml"),
		"pid":         filepath.Join(base, "run", "pueue.pid"),
		"secret":      filepath.Join(base, "data", "shared_secret"),
		"certificate": filepath.Join(base, "data", "certs", "daemon.cert"),
		"key":         filepath.Join(base, "data", "certs", "daemon.key"),
	}
	actual := map[string]string{
		"pueue data":  resolved.PueueDirectory,
		"runtime":     resolved.RuntimeDirectory,
		"alias":       resolved.AliasFile,
		"socket":      resolved.SocketPath,
		"pid":         resolved.PIDPath,
		"secret":      resolved.SecretPath,
		"certificate": resolved.CertificatePath,
		"key":         resolved.KeyPath,
	}
	for name, expected := range want {
		if actual[name] != expected {
			return fmt.Errorf("%w: legacy private supervisor %s path changed", ErrBinding, name)
		}
	}
	if endpoint != "" {
		socketPath, err := savedSocketPath(endpoint)
		if err != nil {
			return err
		}
		if resolved.SocketPath != socketPath {
			return fmt.Errorf("%w: legacy private supervisor socket path changed", ErrBinding)
		}
	} else if err := pathWithin(base, resolved.SocketPath); err != nil {
		return fmt.Errorf("%w: legacy private supervisor endpoint is outside its private directory", ErrBinding)
	}
	if resolved.Host != "127.0.0.1" || resolved.Port != "6924" || resolved.SocketPermissions == nil || *resolved.SocketPermissions != 448 {
		return fmt.Errorf("%w: legacy private supervisor transport settings changed", ErrBinding)
	}
	return nil
}

func retainCleanupUntil(cleanup func() error, err error) error {
	if cleanup == nil {
		return nil
	}
	pending := pendingHandles(err)
	if len(pending) == 0 {
		return cleanup()
	}
	go func() {
		for _, handle := range pending {
			<-handle.Done()
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			log.Printf("delegation-layer: deferred private supervisor cleanup failed: %v", cleanupErr)
		}
	}()
	return nil
}

func savedSocketPath(endpoint string) (string, error) {
	const prefix = "unix:"
	if !strings.HasPrefix(endpoint, prefix) {
		return "", fmt.Errorf("%w: saved supervisor endpoint is not a Unix socket", ErrBinding)
	}
	path := strings.TrimPrefix(endpoint, prefix)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: saved supervisor endpoint is invalid", ErrBinding)
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return "", err
	}
	return path, nil
}

func pathWithin(base, path string) error {
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrBinding
	}
	return nil
}

func createMigrationConfig(base string, data []byte) (path string, cleanup func() error, resultErr error) {
	file, path, err := createPrivateStage(base, ".migration-pueue-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() error {
		return errors.Join(os.Remove(path), syncPrivateDirectory(base))
	}
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return "", nil, errors.Join(fmt.Errorf("%w: write migration supervisor config: %w", ErrConfiguration, writeErr), file.Close(), cleanup())
	}
	if err = privateBarrierFile(file); err != nil {
		return "", nil, errors.Join(fmt.Errorf("%w: sync migration supervisor config: %w", ErrConfiguration, err), file.Close(), cleanup())
	}
	if err = file.Close(); err != nil {
		return "", nil, errors.Join(fmt.Errorf("%w: close migration supervisor config: %w", ErrConfiguration, err), cleanup())
	}
	if err = syncPrivateDirectory(base); err != nil {
		return "", nil, errors.Join(err, cleanup())
	}
	return path, cleanup, nil
}

func stopRecoveredSupervisor(parent context.Context, client *Client, timeout time.Duration, prior ...error) error {
	if client == nil {
		return ErrConfiguration
	}
	if parent == nil {
		return context.Canceled
	}
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	process, processPresent, processErr := client.privateDaemonProcessIdentity()
	if processErr != nil {
		return processErr
	}
	var cleanupErr error
	for _, pending := range pendingHandles(errors.Join(prior...)) {
		_, err := awaitPending(ctx, pending)
		if pendingFrom(err) != nil {
			return fmt.Errorf("wait for recovered supervisor probe: %w", err)
		}
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := client.Shutdown(ctx); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("cleanup recovered supervisor: %w", err))
	}
	if processPresent {
		return errors.Join(cleanupErr, client.waitUnavailableWithProcess(ctx, process))
	}
	return errors.Join(cleanupErr, client.WaitUnavailable(ctx))
}

func validateSavedSupervisorRef(saved task.SupervisorRef) error {
	if err := validatePrivateSupervisorRef(saved); err != nil {
		return err
	}
	if saved.ClientExecutable == "" {
		return errors.New("saved supervisor client executable is unavailable")
	}
	return nil
}

func savedDaemon(saved task.SupervisorRef) (string, error) {
	path := saved.DaemonExecutable
	if path == "" {
		path = filepath.Join(filepath.Dir(saved.ClientExecutable), "pueued")
	}
	resolved, err := canonicalExecutable(path)
	if err != nil {
		return "", fmt.Errorf("%w: saved supervisor daemon is unavailable: %w", ErrBinding, err)
	}
	return resolved, nil
}
