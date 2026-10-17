package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrivateConfigPath returns the canonical config path owned by a state root.
func PrivateConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, "pueue.yml")
}

// IsPrivateConfig reports whether configPath is the state-rooted private
// supervisor config used by the released CLI.
func IsPrivateConfig(stateRoot, configPath string) bool {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || configPath != PrivateConfigPath(stateRoot) {
		return false
	}
	if isDelegateStateRoot(stateRoot) {
		return true
	}
	// Before the store is initialized, the reserved path is still needed to
	// reject an explicit --pueue-config collision. Once a config exists, only
	// a validated Delegate root may claim it; an existing file under an
	// arbitrary <dir>/.supervisor path is external state.
	_, err := os.Lstat(configPath)
	return errors.Is(err, os.ErrNotExist)
}

func isDelegateStateRoot(stateRoot string) bool {
	data, err := readRegular(filepath.Join(stateRoot, "root.json"), MaxControlBytes)
	if err != nil {
		return false
	}
	var record task.RootRecord
	return task.DecodeStrict(data, &record) == nil && task.ValidateRootRecord(&record) == nil
}

func privateStateRoot(configPath string) (string, bool) {
	supervisorDirectory := filepath.Dir(configPath)
	if filepath.Base(supervisorDirectory) != privateSupervisorDirectory {
		return "", false
	}
	stateRoot := filepath.Dir(supervisorDirectory)
	return stateRoot, IsPrivateConfig(stateRoot, configPath)
}

func (c *Client) withPrivateBootstrapLock(ctx context.Context, callback func(context.Context) error) error {
	return c.withPrivateSupervisorLock(ctx, callback, WithBootstrapLockContext)
}

func (c *Client) withPrivateSupervisorLease(ctx context.Context, callback func(context.Context) error) error {
	return c.withPrivateSupervisorLock(ctx, callback, WithSupervisorLeaseContext)
}

func (c *Client) withPrivateSupervisorLock(ctx context.Context, callback func(context.Context) error, acquire func(context.Context, string, func(context.Context) error) error) error {
	if c == nil {
		return ErrConfiguration
	}
	if callback == nil {
		return ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return callback(ctx)
	}
	return acquire(ctx, stateRoot, callback)
}

func (c *Client) privateControlContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if c == nil {
		return nil, nil, ErrConfiguration
	}
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	timeout := PrivateControlTimeout(c.options)
	if timeout <= 0 {
		return nil, nil, ErrConfiguration
	}
	controlContext, cancel := context.WithTimeout(ctx, timeout)
	return controlContext, cancel, nil
}

// BindPrivate creates or reuses a supervisor owned by one Delegation Layer
// state root. The daemon is started only when the private endpoint is not
// ready; the caller never owns provider process lifetime.
func BindPrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	return bindPrivate(ctx, clientExecutable, daemonExecutable, stateRoot, options, nil)
}

// PreparePrivate binds the current private supervisor without starting its
// daemon. Callers must hold WithBootstrapLock before they later call
// StartPrivate; this split lets migration inspect a saved supervisor before a
// replacement can touch the shared queue.
func PreparePrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	bootstrapContext, cancel, options, err := privateBootstrapOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	options, resolution, clientPath, daemonPath, base, lock, err := preparePrivateBootstrap(bootstrapContext, clientExecutable, daemonExecutable, stateRoot, options)
	if err != nil {
		return nil, err
	}
	defer finishPrivateBootstrap(&lock, &resultErr)
	return preparePrivateClient(bootstrapContext, stateRoot, options, resolution, clientPath, daemonPath, base, false, true)
}

// ProbePrivate binds and observes the state-rooted endpoint without starting
// it or cleaning any supervisor rows. Read-only maintenance uses this probe so
// a terminal task observation cannot wake a stopped queue or reap ambiguous
// work while trying to remove one completed row. Its callers require a complete
// generation before attempting maintenance.
func ProbePrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (*Client, bool, error) {
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeSnapshot, privateGenerationComplete, nil)
}

// ProbePrivateWithGate keeps an observation gate in the same generation lease
// as readiness. It can observe the published predecessor of a prepared migration;
// that readiness grants neither admission nor mutation authority.
func ProbePrivateWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeSnapshot, privateGenerationObservable, gate)
}

// ProbePrivateGroupsWithGate uses Pueue's bounded group response for an
// admission that needs only endpoint liveness and default-group scheduling.
// It is deliberately narrower than ProbePrivateWithGate: bootstrap,
// recovery, migration, and task reconciliation retain their full status proof.
func ProbePrivateGroupsWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeGroups, privateGenerationComplete, gate)
}

// ProbePrivateEndpointWithGate verifies the published private generation and
// checks its Unix endpoint without launching a Pueue control process. The gate
// may trust only durable records tied to the returned binding. Any incomplete,
// changed, or unavailable state returns ready=false so the caller can enter its
// exclusive lifecycle path and perform a protocol-level readiness check.
func ProbePrivateEndpointWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, gate func(context.Context, *Client) (bool, error)) (*Client, bool, error) {
	var wrapped func(context.Context, *Client, QueueSnapshot) (bool, error)
	if gate != nil {
		wrapped = func(checkContext context.Context, client *Client, _ QueueSnapshot) (bool, error) {
			return gate(checkContext, client)
		}
	}
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeEndpoint, privateGenerationComplete, wrapped)
}

// ProbePrivateGroupsWithBootstrapLock observes an already-published private
// supervisor while the caller holds WithBootstrapLock for stateRoot. It proves
// the current binding and endpoint through Pueue's bounded group response, so
// a repair limited to ordinary scheduling does not deserialize unrelated task
// rows. A missing, changed, or incomplete generation returns ready=false; the
// caller must use the full lifecycle path before it can start or migrate one.
func ProbePrivateGroupsWithBootstrapLock(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, groups map[string]Group, ready bool, resultErr error) {
	probeContext, cancel, options, err := privateProbeOptions(ctx, options)
	if err != nil {
		return nil, nil, false, err
	}
	defer cancel()
	var snapshot QueueSnapshot
	client, snapshot, ready, resultErr = probePrivateReady(probeContext, clientExecutable, daemonExecutable, stateRoot, options, privateProbeGroups, privateGenerationComplete)
	if resultErr != nil || !ready {
		if client != nil {
			closeErr := client.CloseAfterPending(resultErr)
			if resultErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
			} else {
				resultErr = closeErr
			}
			client = nil
		}
		return nil, nil, false, resultErr
	}
	return client, snapshot.Groups, true, nil
}

func probePrivateWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, scope privateProbeScope, required privateGenerationPhase, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (client *Client, ready bool, resultErr error) {
	probeContext, cancel, options, err := privateProbeOptions(ctx, options)
	if err != nil {
		return nil, false, err
	}
	defer cancel()
	leasePresent, err := privateBootstrapLockPresent(stateRoot)
	if err != nil {
		return nil, false, err
	}
	if !leasePresent {
		// A fresh or incomplete root has no published generation to observe.
		// Leave it untouched; the admission owner will enter the exclusive
		// bootstrap path and provision the lifecycle boundary if needed.
		return nil, false, nil
	}
	resultErr = WithSupervisorLeaseContext(probeContext, stateRoot, func(lockedContext context.Context) error {
		var probeErr error
		var snapshot QueueSnapshot
		client, snapshot, ready, probeErr = probePrivateReady(lockedContext, clientExecutable, daemonExecutable, stateRoot, options, scope, required)
		if probeErr != nil || !ready || gate == nil {
			return probeErr
		}
		ready, probeErr = gate(lockedContext, client, snapshot)
		return probeErr
	})
	if resultErr != nil && client != nil {
		resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr))
		client = nil
	}
	return client, ready, resultErr
}

func probePrivateReady(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, scope privateProbeScope, required privateGenerationPhase) (*Client, QueueSnapshot, bool, error) {
	published, found, err := completePrivateProbeBinding(stateRoot, required)
	if err != nil || !found {
		return nil, QueueSnapshot{}, false, err
	}
	resolution, configResolution, err := privateProbeResolutions(published, options)
	if err != nil {
		return nil, QueueSnapshot{}, false, err
	}
	configPath := PrivateConfigPath(stateRoot)
	resolved, err := readPrivateConfig(configPath, filepath.Dir(configPath), configResolution)
	if err != nil {
		return nil, QueueSnapshot{}, false, err
	}
	directoriesReady, err := privateProbeDirectoriesReady(resolved)
	if err != nil || !directoriesReady {
		return nil, QueueSnapshot{}, false, err
	}
	client, snapshot, ready, err := probePublishedPrivateClient(ctx, published, clientExecutable, daemonExecutable, configPath, resolution, options, scope)
	if err != nil || client == nil {
		return client, QueueSnapshot{}, false, err
	}
	if err = verifyPrivateSupervisorBinding(stateRoot, client.Binding(), ready); err != nil {
		return nil, QueueSnapshot{}, false, errors.Join(err, client.CloseAfterPending(err))
	}
	return client, snapshot, ready, nil
}

func completePrivateProbeBinding(stateRoot string, required privateGenerationPhase) (task.SupervisorRef, bool, error) {
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return task.SupervisorRef{}, false, err
	}
	if facts.publishedErr != nil || !facts.publishedFound {
		return task.SupervisorRef{}, false, facts.publishedErr
	}
	if required == privateGenerationComplete {
		complete, completeErr := facts.complete(facts.published, false)
		return facts.published, complete, completeErr
	}
	phase, err := facts.phase(facts.published)
	return facts.published, phase >= required, err
}

func privateProbeResolutions(published task.SupervisorRef, options Options) (ResolutionContext, ResolutionContext, error) {
	resolution, err := privateResolution(options)
	if err != nil {
		return ResolutionContext{}, ResolutionContext{}, err
	}
	configResolution := resolution
	publishedResolution, present, err := resolutionFromBinding(published)
	if err != nil {
		return ResolutionContext{}, ResolutionContext{}, err
	}
	if present {
		// The published generation owns the environment used to resolve its
		// config and endpoint. Staged successor inventory cannot change it.
		configResolution = publishedResolution
	}
	return resolution, configResolution, nil
}

func privateProbeDirectoriesReady(resolved ResolvedConfig) (bool, error) {
	for _, directory := range []string{resolved.PueueDirectory, resolved.RuntimeDirectory} {
		info, err := os.Stat(directory)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, fmt.Errorf("%w: private supervisor directory is not a directory", ErrConfiguration)
		}
		if err = rejectSymlinkComponents(directory); err != nil {
			return false, err
		}
	}
	return true, nil
}

// A published version describes the exact executable bytes in its binding.
// Reopening that same pair checks the bytes and endpoint again without a
// redundant version subprocess. A different supplied pair remains successor
// inventory; this probe always reopens the exact published pair.
func probePublishedPrivateClient(ctx context.Context, published task.SupervisorRef, clientExecutable, daemonExecutable, configPath string, resolution ResolutionContext, options Options, scope privateProbeScope) (*Client, QueueSnapshot, bool, error) {
	options, resolution, pairChanged, resolutionChanged, err := publishedPrivateProbeOptions(published, clientExecutable, daemonExecutable, resolution, options)
	if err != nil {
		return nil, QueueSnapshot{}, false, err
	}
	if !pairChanged && resolutionChanged {
		return probeChangedResolutionPrivateClient(ctx, clientExecutable, daemonExecutable, configPath, options, scope)
	}
	return probeExactPublishedPrivateClient(ctx, published, resolution, options, scope)
}

func publishedPrivateProbeOptions(published task.SupervisorRef, clientExecutable, daemonExecutable string, resolution ResolutionContext, options Options) (Options, ResolutionContext, bool, bool, error) {
	publishedResolution, present, err := resolutionFromBinding(published)
	if err != nil {
		return Options{}, ResolutionContext{}, false, false, err
	}
	pairChanged := published.ClientExecutable != clientExecutable || published.DaemonExecutable != daemonExecutable
	resolutionChanged := present && !sameEnvironmentResolution(publishedResolution, resolution)
	if !pairChanged || !present {
		return options, resolution, pairChanged, resolutionChanged, nil
	}
	resolution = publishedResolution
	options.Resolution = &resolution
	options.resolutionPinned = true
	if options.Environment == nil || len(options.Environment) > 0 {
		options.Environment = environmentForResolution(options.Environment, resolution)
	}
	return options, resolution, pairChanged, resolutionChanged, nil
}

func probeChangedResolutionPrivateClient(ctx context.Context, clientExecutable, daemonExecutable, configPath string, options Options, scope privateProbeScope) (*Client, QueueSnapshot, bool, error) {
	client, ready, err := bindPrivateClientWithCleanup(ctx, clientExecutable, daemonExecutable, configPath, options, false)
	if err != nil || !ready || client == nil {
		return client, QueueSnapshot{}, ready, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return client, QueueSnapshot{}, false, err
	}
	if scope == privateProbeGroups {
		return client, QueueSnapshot{Groups: snapshot.Groups}, true, nil
	}
	if scope == privateProbeSnapshot {
		return client, snapshot, true, nil
	}
	return nil, QueueSnapshot{}, false, errors.Join(ErrConfiguration, client.CloseAfterPending(nil))
}

func probeExactPublishedPrivateClient(ctx context.Context, published task.SupervisorRef, resolution ResolutionContext, options Options, scope privateProbeScope) (*Client, QueueSnapshot, bool, error) {
	options.Resolution = &resolution
	client, err := NewClient(published, options)
	if err != nil {
		return nil, QueueSnapshot{}, false, err
	}
	if err = client.verify(ctx); err != nil {
		if errors.Is(err, ErrBinding) {
			return nil, QueueSnapshot{}, false, nil
		}
		return nil, QueueSnapshot{}, false, err
	}
	if err = attachPrivateDaemon(client, published.DaemonExecutable); err != nil {
		return nil, QueueSnapshot{}, false, err
	}
	if client.binding.DaemonSHA256 != published.DaemonSHA256 {
		return nil, QueueSnapshot{}, false, nil
	}
	switch scope {
	case privateProbeEndpoint:
		ready, readyErr := privateClientReadyEndpoint(ctx, client)
		return client, QueueSnapshot{}, ready, readyErr
	case privateProbeGroups:
		groups, ready, groupErr := privateClientReadyGroups(ctx, client)
		return client, QueueSnapshot{Groups: groups}, ready, groupErr
	case privateProbeSnapshot:
		snapshot, ready, readyErr := privateClientReadySnapshotWithCleanup(ctx, client, false)
		return client, snapshot, ready, readyErr
	default:
		return nil, QueueSnapshot{}, false, errors.Join(ErrConfiguration, client.CloseAfterPending(nil))
	}
}

func preparePrivateClient(ctx context.Context, stateRoot string, options Options, resolution ResolutionContext, clientPath, daemonPath, base string, migrationOnly, cleanupSuccessful bool) (*Client, error) {
	configPath, _, err := privateBindingConfig(stateRoot, base, resolution, nil)
	if err != nil {
		return nil, err
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, nil); err != nil {
		return nil, err
	}
	var client *Client
	if migrationOnly {
		client, err = Bind(ctx, clientPath, configPath, options)
		if err == nil {
			err = attachPrivateDaemon(client, daemonPath)
		}
	} else {
		client, _, err = bindPrivateClientWithCleanup(ctx, clientPath, daemonPath, configPath, options, cleanupSuccessful)
	}
	if err != nil {
		return nil, err
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, nil); err != nil {
		return nil, err
	}
	return client, nil
}
