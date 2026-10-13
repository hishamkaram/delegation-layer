package pueue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	privateSupervisorDirectory           = ".supervisor"
	privateMigrationRecordName           = "migration.json"
	privateMigrationHistoryRecordName    = "migration-history.json"
	privateSupervisorBindingRecordName   = "supervisor.identity.json"
	privateBootstrapRecordName           = "bootstrap.pending"
	privateIdentityPublicationRecordName = "identity.pending.json"
	privateSchedulingEvidenceRecordName  = "ordinary-scheduling.json"
	privateTransitionLockName            = "transition.lock"
)

var (
	// ErrPrivateSupervisorMigrationBusy means the private supervisor still
	// owns queued, running, or uncertain work and must remain alive.
	ErrPrivateSupervisorMigrationBusy = errors.New("private supervisor migration has active work")
	// ErrPrivateSupervisorMigrationRequired means private supervisor launch
	// provenance could not be migrated safely by the current admission path.
	ErrPrivateSupervisorMigrationRequired = errors.New("private supervisor migration is required")
	// ErrPrivateSupervisorNotInitialized means a shared lifecycle observation
	// found no published bootstrap boundary. Callers may elect an exclusive
	// transition to initialize it, but must not create state from the read path.
	ErrPrivateSupervisorNotInitialized = errors.New("private supervisor lifecycle is not initialized")
	errStatusControlLimit              = errors.New("pueue supervisor status output limit exceeded")
	errSupervisorLockBusy              = errors.New("private supervisor lock is busy")
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
// work while trying to remove one completed row.
func ProbePrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (*Client, bool, error) {
	return ProbePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, nil)
}

// ProbePrivateWithGate keeps the application's final admission check in the
// same generation lease as readiness. False readiness asks the caller to use
// the exclusive recovery path; no incomplete identity is repaired here.
func ProbePrivateWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeSnapshot, gate)
}

// ProbePrivateGroupsWithGate uses Pueue's bounded group response for an
// admission that needs only endpoint liveness and default-group scheduling.
// It is deliberately narrower than ProbePrivateWithGate: bootstrap,
// recovery, migration, and task reconciliation retain their full status proof.
func ProbePrivateGroupsWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeGroups, gate)
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
	return probePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, privateProbeEndpoint, wrapped)
}

type privateProbeScope uint8

const (
	privateProbeSnapshot privateProbeScope = iota
	privateProbeGroups
	privateProbeEndpoint
)

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
	client, snapshot, ready, resultErr = probePrivateReady(probeContext, clientExecutable, daemonExecutable, stateRoot, options, privateProbeGroups)
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

func probePrivateWithGate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, scope privateProbeScope, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (client *Client, ready bool, resultErr error) {
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
		client, snapshot, ready, probeErr = probePrivateReady(lockedContext, clientExecutable, daemonExecutable, stateRoot, options, scope)
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

func probePrivateReady(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, scope privateProbeScope) (*Client, QueueSnapshot, bool, error) {
	published, found, err := completePrivateProbeBinding(stateRoot)
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

func completePrivateProbeBinding(stateRoot string) (task.SupervisorRef, bool, error) {
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return task.SupervisorRef{}, false, err
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, published)
	return published, complete, err
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
		// The completed publication owns the environment used to resolve its
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
// inventory; this probe always reopens the completed published pair.
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

// WithBootstrapLock serializes a state-root supervisor transition with normal
// private bootstrap. The callback runs while the same lock used by
// BindPrivate is held; it must perform only bounded, state-rooted recovery and
// return without retaining the lock or starting an unrelated supervisor.
func WithBootstrapLock(ctx context.Context, stateRoot string, callback func() error) error {
	if callback == nil {
		return ErrConfiguration
	}
	return WithBootstrapLockContext(ctx, stateRoot, func(context.Context) error { return callback() })
}

// WithBootstrapLockContext is WithBootstrapLock with the acquired generation
// lease attached to the callback context. Supervisor controls started with that
// context transfer a duplicate lease to their one-shot process owner.
func WithBootstrapLockContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return withProvisionedPrivateLockContext(ctx, stateRoot, "bootstrap.lock", unix.LOCK_EX, callback)
}

// TryWithPrivateTransition elects one state-root transition owner without
// competing with shared supervisor leases. A false acquisition means another
// caller is already preparing this root; callers should wait for that
// transition before probing again instead of spinning through more controls.
func TryWithPrivateTransition(ctx context.Context, stateRoot string, callback func() error) (bool, error) {
	if callback == nil {
		return false, ErrConfiguration
	}
	if err := ensurePrivateTransitionElection(ctx, stateRoot); err != nil {
		return false, err
	}
	acquired := false
	err := withPrivateTransitionLock(ctx, stateRoot, unix.LOCK_EX|unix.LOCK_NB, func() error {
		acquired = true
		return callback()
	})
	return acquired, err
}

// WaitForPrivateTransition waits for the currently elected state-root
// transition, if any, to release its election lock. It never changes the
// supervisor itself; callers re-probe their required live state afterward.
func WaitForPrivateTransition(ctx context.Context, stateRoot string) error {
	present, err := privateTransitionLockPresent(stateRoot)
	if err != nil || !present {
		return err
	}
	return withPrivateTransitionLock(ctx, stateRoot, unix.LOCK_EX, func() error { return nil })
}

// WithSupervisorLease allows independent controls to share a stable supervisor
// generation. Lifecycle transitions require WithBootstrapLock and wait for all
// leases, including leases retained by unfinished commands, to drain.
func WithSupervisorLease(ctx context.Context, stateRoot string, callback func() error) error {
	if callback == nil {
		return ErrConfiguration
	}
	return WithSupervisorLeaseContext(ctx, stateRoot, func(context.Context) error { return callback() })
}

// WithSupervisorLeaseContext is WithSupervisorLease with the acquired
// generation lease attached to the callback context.
func WithSupervisorLeaseContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return withPublishedPrivateLockContext(ctx, stateRoot, "bootstrap.lock", unix.LOCK_SH, callback)
}

func withProvisionedPrivateLock(ctx context.Context, stateRoot, name string, mode int, callback func() error) error {
	return withPrivateLock(ctx, stateRoot, name, mode, true, callback)
}

func withProvisionedPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, callback func(context.Context) error) error {
	return withPrivateLockContext(ctx, stateRoot, name, mode, true, callback)
}

func withPublishedPrivateLock(ctx context.Context, stateRoot, name string, mode int, callback func() error) error {
	return withPrivateLock(ctx, stateRoot, name, mode, false, callback)
}

func withPublishedPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, callback func(context.Context) error) error {
	return withPrivateLockContext(ctx, stateRoot, name, mode, false, callback)
}

func withPrivateTransitionLock(ctx context.Context, stateRoot string, mode int, callback func() error) error {
	return withPublishedPrivateLock(ctx, stateRoot, privateTransitionLockName, mode, callback)
}

// withPrivateLock either provisions the bootstrap boundary for an exclusive
// lifecycle transition or acquires an already-published lock. Shared leases,
// observations and transition waits must never manufacture state:
// callers first decide whether a mutation is warranted, then enter an
// exclusive bootstrap boundary to provision it.
func withPrivateLock(ctx context.Context, stateRoot, name string, mode int, provision bool, callback func() error) (resultErr error) {
	if callback == nil {
		return ErrConfiguration
	}
	return withPrivateLockContext(ctx, stateRoot, name, mode, provision, func(context.Context) error { return callback() })
}

func withPrivateLockContext(ctx context.Context, stateRoot, name string, mode int, provision bool, callback func(context.Context) error) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if callback == nil || name == "" || filepath.Base(name) != name {
		return ErrConfiguration
	}
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if provision {
		if err := ensurePrivateBase(base); err != nil {
			return err
		}
	}
	present, err := privateLockFilePresent(base, name)
	if err != nil {
		return err
	}
	if !present {
		if !provision {
			return ErrPrivateSupervisorNotInitialized
		}
		return fmt.Errorf("%w: private supervisor lock is missing", ErrConfiguration)
	}
	lock, err := acquireExistingSupervisorLock(ctx, filepath.Join(base, name), mode)
	if err != nil {
		if mode&unix.LOCK_NB != 0 && errors.Is(err, errSupervisorLockBusy) {
			return nil
		}
		return err
	}
	defer finishPrivateBootstrap(&lock, &resultErr)
	return callback(withGenerationControlLease(ctx, lock))
}

func privateBootstrapLockPresent(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	return privateLockFilePresent(filepath.Join(stateRoot, privateSupervisorDirectory), "bootstrap.lock")
}

// ensurePrivateTransitionElection installs the election lock only while the
// caller owns the bootstrap lock. A shared probe may discover a repair need,
// but it must never create a state-root entry before it has become the
// exclusive transition owner. Existing roots get this one-time lock upgrade
// through the same bootstrap boundary.
func ensurePrivateTransitionElection(ctx context.Context, stateRoot string) error {
	present, err := privateTransitionLockPresent(stateRoot)
	if err != nil || present {
		return err
	}
	return WithBootstrapLock(ctx, stateRoot, func() error {
		base := filepath.Join(stateRoot, privateSupervisorDirectory)
		if err := ensurePrivateBase(base); err != nil {
			return err
		}
		return ensurePrivateLockFile(base, privateTransitionLockName)
	})
}

func privateTransitionLockPresent(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	return privateLockFilePresent(filepath.Join(stateRoot, privateSupervisorDirectory), privateTransitionLockName)
}

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
	if err := recoverPrivateIdentityPublication(stateRoot); err != nil {
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
	if err = restoreExpectedPrivateSupervisorBinding(stateRoot, expected); err != nil {
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

func restoreExpectedPrivateSupervisorBinding(stateRoot string, expected *task.SupervisorRef) error {
	if expected == nil {
		return nil
	}
	return restorePrivateSupervisorBindingFromSaved(stateRoot, *expected)
}

// Fresh bootstrap observes the supervisor once while binding the client, twice
// while checking an existing endpoint, and twice while checking a daemon just
// started by this process. Saved-binding recovery adds two exact binding
// probes before a restart, so the shared budget covers that maximum path.
const (
	privateBootstrapObservationPhases      = 7
	privateReconciliationObservationPhases = 2
	// Queue maintenance can wait for the current generation lease, observe the
	// complete queue, and remove the exact authorized rows. Give each phase one
	// observation window while keeping the window independent from a longer
	// admission timeout.
	privateQueueMaintenanceObservationPhases = 3
	// Migration performs one current-pair preparation, legacy binding and
	// queue inspection, shutdown acknowledgement and endpoint drain, then
	// successor startup and readiness reconciliation. Keep this explicit so
	// the outer deadline is derived from the full transition rather than one
	// bootstrap attempt.
	privateMigrationObservationPhases = 8
)

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
	return nil
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

// RecoverPrivate reopens a saved private task through the state-rooted
// supervisor. It first adopts a live, completed successor proven to own the
// saved generation; a stopped endpoint is restarted only from that exact
// completed publication. A genuinely legacy root may restart only the exact
// saved pair when its daemon identity proves the same generation. The durable
// task binding remains unchanged while the runtime binding names the active
// generation actually used for control.
func RecoverPrivate(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, error) {
	if !IsPrivateConfig(stateRoot, saved.ConfigPath) {
		return nil, fmt.Errorf("%w: supervisor config is not private to the state root", ErrConfiguration)
	}
	if err := validatePrivateSupervisorRef(saved); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	bootstrapContext, cancel, options, err := privateBootstrapOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	options, err = optionsForSavedBinding(saved, options)
	if err != nil {
		return nil, err
	}
	// Validate the exact saved config before any endpoint or client probe. In
	// particular, an absent saved config must remain ErrBinding rather than
	// becoming Bind's ErrConfiguration after a probe has already run.
	if err = validateSavedPrivateConfig(stateRoot, saved, options); err != nil {
		return nil, err
	}
	if probed, probeErr := probeSavedPrivate(bootstrapContext, stateRoot, saved, clientExecutable, daemonExecutable, options); probeErr != nil || probed != nil {
		return probed, probeErr
	}
	var recovered *Client
	if err = WithBootstrapLockContext(bootstrapContext, stateRoot, func(lockedContext context.Context) error {
		// A pending explicit repair may have preserved the old config under a
		// root-owned alias. Reopen that exact endpoint first so its queued work
		// remains recoverable; this is the only recovery exception to the
		// current state-root config.
		var found bool
		var recoveryErr error
		recovered, _, found, recoveryErr = recoverPrivateRecoveryBinding(lockedContext, stateRoot, saved, clientExecutable, daemonExecutable, options)
		if recoveryErr != nil {
			return recoveryErr
		}
		if found {
			return nil
		}
		recovered, err = recoverPrivateGenerationWithLock(lockedContext, stateRoot, saved, clientExecutable, daemonExecutable, options)
		return err
	}); err != nil {
		return nil, err
	}
	return recovered, nil
}

func validateSavedPrivateConfig(stateRoot string, saved task.SupervisorRef, options Options) error {
	resolution, err := privateResolution(options)
	if err != nil {
		return err
	}
	_, _, err = privateBindingConfig(stateRoot, filepath.Join(stateRoot, privateSupervisorDirectory), resolution, &saved)
	return err
}

func probeSavedPrivate(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, error) {
	probed, ready, err := ProbePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, func(checkContext context.Context, client *Client, _ QueueSnapshot) (bool, error) {
		current, found, resolveErr := ResolvePrivateSupervisorSuccessorWithLock(checkContext, stateRoot, saved)
		if resolveErr != nil {
			return false, resolveErr
		}
		return found && privateFullGenerationMatches(client.Binding(), current), nil
	})
	if err != nil {
		return nil, bindingProbeError(err)
	}
	if ready {
		probed.binding = saved
		return probed, nil
	}
	if probed != nil {
		return nil, probed.Close()
	}
	return nil, nil
}

func recoverPrivateGenerationWithLock(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, error) {
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: read completed private supervisor generation: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !found {
		return recoverLegacyPrivateGeneration(ctx, stateRoot, saved, options)
	}

	current, connected, err := ResolvePrivateSupervisorSuccessorWithLock(ctx, stateRoot, saved)
	if err != nil {
		return nil, err
	}
	if !connected || !privateFullGenerationMatches(current, published) {
		return nil, fmt.Errorf("%w: saved private supervisor is unrelated to the completed active generation", ErrPrivateSupervisorMigrationRequired)
	}

	client, prepared, prepareErr := PreparePublishedPrivateWithLock(ctx, stateRoot, options)
	if prepareErr == nil && prepared {
		if err = client.StartPrivate(ctx, stateRoot); err != nil {
			return nil, errors.Join(err, client.CloseAfterPending(err))
		}
		client.binding = saved
		return client, nil
	}
	if client != nil {
		prepareErr = errors.Join(prepareErr, client.CloseAfterPending(prepareErr))
	}
	if complete, completeErr := privateSupervisorMigrationComplete(stateRoot, published, false); completeErr != nil {
		return nil, completeErr
	} else if !complete {
		return nil, errors.Join(
			fmt.Errorf("%w: completed private supervisor transition changed during recovery", ErrPrivateSupervisorMigrationRequired),
			prepareErr,
		)
	}

	// The completed pair may have disappeared after publication. A compatible
	// pair can still inspect the live endpoint, but it is never allowed to start
	// a stopped generation or publish its own identity.
	compatible, ready, probeErr := probePrivateCompatiblePair(ctx, saved, published, clientExecutable, daemonExecutable, options)
	if probeErr != nil {
		return nil, probeErr
	}
	if ready {
		return compatible, nil
	}
	return nil, fmt.Errorf("%w: completed private supervisor pair is unavailable for restart", ErrPrivateSupervisorMigrationRequired)
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
	if !found || saved.DaemonExecutable == "" || saved.DaemonSHA256 == "" || !privateDaemonIdentityMatchesBinding(identity, saved) {
		return nil, fmt.Errorf("%w: legacy private supervisor generation cannot be proven from saved identity", ErrPrivateSupervisorMigrationRequired)
	}

	// A missing completed binding is recoverable only through the exact saved
	// pair. The caller's pair is intentionally ignored in this branch.
	client, ready, err := bindPrivateClientWithCleanup(ctx, saved.ClientExecutable, saved.DaemonExecutable, saved.ConfigPath, options, false)
	if err != nil {
		return nil, fmt.Errorf("%w: reopen exact legacy private supervisor pair: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !privateFullGenerationMatches(client.Binding(), saved) {
		return nil, errors.Join(
			fmt.Errorf("%w: exact legacy private supervisor generation changed", ErrPrivateSupervisorMigrationRequired),
			client.Close(),
		)
	}
	if !ready {
		if err = client.startPrivate(ctx); err != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: restart exact legacy private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err),
				client.CloseAfterPending(err),
			)
		}
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
func RecoverSaved(ctx context.Context, saved task.SupervisorRef, options Options) (*Client, bool, error) {
	return RecoverSavedWithPair(ctx, saved, "", "", options)
}

// RecoverSavedWithPair is the migration form of RecoverSaved. It uses the
// saved client and daemon while those installation paths still exist, then
// falls back to a caller-supplied verified pair when an upgrade removed them.
// The durable binding remains the saved supervisor; the pair only supplies the
// executable implementation used to inspect or restart that same endpoint.
func RecoverSavedWithPair(ctx context.Context, saved task.SupervisorRef, clientExecutable, daemonExecutable string, options Options) (*Client, bool, error) {
	if err := validateSavedSupervisorRef(saved); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	stateRoot, private := privateStateRoot(saved.ConfigPath)
	if private {
		resolvedClient, resolveErr := selectRecoveryExecutable(saved.ClientExecutable, clientExecutable)
		if resolveErr != nil {
			return nil, false, resolveErr
		}
		resolvedDaemon, _, resolveErr := selectRecoveryDaemon(saved, daemonExecutable)
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
	clientExecutable, err = selectRecoveryExecutable(saved.ClientExecutable, clientExecutable)
	if err != nil {
		return nil, false, err
	}
	client, err := bindSavedForMigrationWithClient(ctx, saved, clientExecutable, options)
	if err != nil {
		return nil, false, err
	}
	if private {
		// Keep the verified predecessor daemon identity on the recovered client
		// even when its endpoint is already reachable. Migration must prove that
		// the process it shuts down is the saved supervisor before waiting for
		// the endpoint and process to disappear.
		daemon, digest, daemonErr := selectRecoveryDaemon(saved, daemonExecutable)
		if daemonErr != nil {
			return nil, false, errors.Join(daemonErr, client.Close())
		}
		client.runtimeBinding.DaemonExecutable = daemon
		client.runtimeBinding.DaemonSHA256 = digest
	}
	ready, err := client.availableForBootstrap(ctx)
	if err != nil {
		return nil, false, errors.Join(err, client.CloseAfterPending(err))
	}
	if ready {
		return client, false, nil
	}

	daemon, digest, err := selectRecoveryDaemon(saved, daemonExecutable)
	if err != nil {
		return nil, false, errors.Join(err, client.Close())
	}
	client.runtimeBinding.DaemonExecutable = daemon
	client.runtimeBinding.DaemonSHA256 = digest
	if err = startDaemon(daemon, digest, client.runtimeBinding.ConfigPath, filepath.Dir(client.runtimeBinding.ConfigPath), options); err != nil {
		return nil, false, errors.Join(err, client.Close())
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		// Readiness failure is inconclusive: no successful private liveness
		// observation has proved that the recovered endpoint is safe to stop. Keep it alive and
		// leave a durable private recovery alias for the next bootstrap.
		combinedErr := err
		if private {
			aliasErr := client.PersistPrivateRecoveryBinding(stateRoot)
			combinedErr = errors.Join(combinedErr, aliasErr)
		}
		return nil, true, errors.Join(combinedErr, client.CloseAfterPending(combinedErr))
	}
	return client, true, nil
}

func selectRecoveryExecutable(saved, fallback string) (string, error) {
	if saved != "" {
		if _, err := os.Stat(saved); err == nil {
			return saved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: inspect saved supervisor executable: %w", ErrBinding, err)
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("%w: saved supervisor executable is unavailable", ErrBinding)
	}
	return fallback, nil
}

func selectRecoveryDaemon(saved task.SupervisorRef, fallback string) (string, string, error) {
	if saved.DaemonExecutable != "" {
		if _, err := os.Stat(saved.DaemonExecutable); err == nil {
			resolved, digest, savedErr := savedDaemon(saved)
			if savedErr == nil {
				return resolved, digest, nil
			}
			if fallback == "" {
				return "", "", savedErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w: inspect saved supervisor daemon: %w", ErrBinding, err)
		}
	}
	if fallback == "" {
		return savedDaemon(saved)
	}
	resolved, err := canonicalExecutable(fallback)
	if err != nil {
		return "", "", fmt.Errorf("%w: fallback supervisor daemon is unavailable: %w", ErrBinding, err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: hash fallback supervisor daemon: %w", ErrBinding, err)
	}
	return resolved, digest, nil
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
	if err = pathWithin(resolved.RuntimeDirectory, socketPath); err != nil {
		return nil, fmt.Errorf("%w: legacy private supervisor endpoint is outside its runtime directory", ErrBinding)
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
		return nil, errors.Join(err, retainCleanupUntil(cleanup, err))
	}
	if migration.Binding().Endpoint != saved.Endpoint {
		return nil, errors.Join(ErrBinding, cleanup())
	}
	migration.binding = saved
	migration.cleanup = cleanup
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
	} else if err := pathWithin(resolved.RuntimeDirectory, resolved.SocketPath); err != nil {
		return fmt.Errorf("%w: legacy private supervisor endpoint is outside its runtime directory", ErrBinding)
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

func savedDaemon(saved task.SupervisorRef) (string, string, error) {
	path := saved.DaemonExecutable
	if path == "" {
		path = filepath.Join(filepath.Dir(saved.ClientExecutable), "pueued")
	}
	resolved, err := canonicalExecutable(path)
	if err != nil {
		return "", "", fmt.Errorf("%w: saved supervisor daemon is unavailable: %w", ErrBinding, err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: hash saved supervisor daemon: %w", ErrBinding, err)
	}
	if saved.DaemonSHA256 != "" && saved.DaemonSHA256 != digest {
		return "", "", fmt.Errorf("%w: saved supervisor daemon identity changed", ErrBinding)
	}
	return resolved, digest, nil
}

// Shutdown requests graceful termination through the exact bound endpoint.
// It never falls back to a process signal or a different supervisor.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.ShutdownDispatched(ctx)
	return err
}

// ShutdownDispatched reports whether the shutdown command was handed to the
// process observer. A false result means verification or command construction
// failed before the command could be dispatched, so a recovery owner may still
// perform its bounded cleanup. A true result transfers command ownership to
// the observer even when the command later fails or remains in flight.
func (c *Client) ShutdownDispatched(ctx context.Context) (dispatched bool, resultErr error) {
	if c == nil {
		return false, ErrConfiguration
	}
	if err := c.verify(ctx); err != nil {
		return false, err
	}
	result, err := c.command(ctx, nil, "shutdown")
	if pending := pendingFrom(err); pending != nil {
		dispatched = true
		result, err = awaitPending(ctx, pending)
	}
	dispatched = dispatched || result.Started
	if err != nil {
		return dispatched, errors.Join(ErrBinding, err)
	}
	if result.ExitCode != 0 {
		return dispatched, fmt.Errorf("%w: supervisor shutdown was rejected", ErrBinding)
	}
	return dispatched, nil
}

// Reachable performs a bounded, read-only readiness probe and reports whether
// the bound endpoint answered a valid status request. A normal endpoint
// absence is represented by (false, nil); malformed or changed bindings remain
// errors so callers cannot mistake incompatibility for a stopped daemon.
func (c *Client) Reachable(ctx context.Context) (bool, error) {
	return c.available(ctx)
}

// VerifyPrivateMigrationEndpoint checks the endpoint that an explicit
// migration would replace before any successor can be started. A stopped
// daemon with the same persisted launch identity is an ordinary restart, so it
// remains safe to start. A stopped daemon whose identity differs from the
// current launch pair is an upgrade migration; without a durable predecessor
// journal, migration must stop rather than risk running two daemons for one
// queue.
func (c *Client) VerifyPrivateMigrationEndpoint(ctx context.Context, stateRoot string) error {
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	// Migration verification is a lifecycle decision. It requires the full
	// status protocol so a malformed task response cannot be mistaken for a
	// recoverable private supervisor. Admission reuses a separately established
	// bounded group proof only for its already-live scheduling path.
	ready, err := c.availableForBootstrap(ctx)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	_, identityPresent, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return err
	}
	if identityPresent {
		_, bindingPresent, bindingErr := loadPrivateSupervisorBinding(stateRoot)
		if bindingErr != nil {
			return bindingErr
		}
		if bindingPresent {
			return nil
		}
		return fmt.Errorf("%w: private supervisor endpoint is unreachable and its logical predecessor binding is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	bootstrapPending, bootstrapErr := PrivateBootstrapPending(stateRoot)
	if bootstrapErr != nil {
		return bootstrapErr
	}
	if bootstrapPending {
		return nil
	}
	return fmt.Errorf("%w: private supervisor endpoint is unreachable and its predecessor identity is unavailable", ErrPrivateSupervisorMigrationRequired)
}

// WaitUnavailable waits until the exact endpoint no longer answers a valid
// status request and, when the private supervisor's PID is recorded, until
// that process has exited. It is used only after a successful Delegate-owned
// shutdown. Verification errors are returned immediately because they cannot
// prove that the endpoint has disappeared.
func (c *Client) WaitUnavailable(ctx context.Context) error {
	if c == nil {
		return ErrConfiguration
	}
	if ctx == nil {
		return context.Canceled
	}
	pid, pidRecorded, err := c.privateDaemonPID()
	if err != nil {
		return err
	}
	process := privateProcessIdentity{}
	processPresent := false
	if pidRecorded {
		process, processPresent, err = c.privateProcessIdentity(pid)
		if err != nil {
			return err
		}
	}
	return c.waitUnavailable(ctx, process, pidRecorded, processPresent)
}

// WaitUnavailableWithPID waits for a previously captured private daemon PID
// as well as the exact endpoint. Migration captures the PID before sending
// shutdown, because a graceful daemon may remove its PID file before the
// caller starts waiting.
func (c *Client) WaitUnavailableWithPID(ctx context.Context, pid int) error {
	if c == nil {
		return ErrConfiguration
	}
	if pid <= 0 {
		return fmt.Errorf("%w: private supervisor PID is invalid", ErrBinding)
	}
	process, present, err := c.privateProcessIdentity(pid)
	if err != nil {
		return err
	}
	return c.waitUnavailable(ctx, process, true, present)
}

func (c *Client) waitUnavailableWithProcess(ctx context.Context, process privateProcessIdentity) error {
	if process.PID <= 0 || process.StartToken == "" {
		return fmt.Errorf("%w: private supervisor process identity is incomplete", ErrBinding)
	}
	return c.waitUnavailable(ctx, process, true, true)
}

func (c *Client) waitUnavailable(ctx context.Context, process privateProcessIdentity, pidRecorded, processPresent bool) error {
	if ctx == nil {
		return context.Canceled
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		unavailable, err := c.privateEndpointUnavailable(ctx, path, process, pidRecorded, processPresent)
		if err != nil {
			return err
		}
		if unavailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) privateEndpointUnavailable(ctx context.Context, path string, process privateProcessIdentity, pidRecorded, processPresent bool) (bool, error) {
	unavailable, refused, err := privateEndpointProbe(ctx, path)
	if err != nil {
		return false, err
	}
	processUnavailable := true
	if processPresent {
		processUnavailable, err = c.privateProcessUnavailable(process)
		if err != nil {
			return false, err
		}
	}
	if refused && pidRecorded && processUnavailable {
		unavailable, err = c.removeStalePrivateEndpoint(ctx, path, process)
		if err != nil {
			return false, err
		}
	}
	return unavailable && processUnavailable, nil
}

// removeStalePrivateEndpoint removes only a state-rooted Unix socket whose
// recorded daemon is already gone. Callers reach this through the bootstrap
// transition lock, so a Delegate-owned successor cannot race the ownership
// check and lose its live endpoint.
func (c *Client) removeStalePrivateEndpoint(ctx context.Context, path string, process privateProcessIdentity) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !isPrivateClient(c) {
		return false, nil
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return false, nil
	}
	runDirectory := filepath.Join(stateRoot, privateSupervisorDirectory, "run")
	if err := pathWithin(runDirectory, path); err != nil {
		return false, fmt.Errorf("%w: private supervisor socket is outside its runtime directory", ErrBinding)
	}
	if unavailable, err := c.privateProcessUnavailable(process); err != nil {
		return false, err
	} else if !unavailable {
		return false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect stale private supervisor socket: %w", ErrBinding, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("%w: private supervisor endpoint is not a Unix socket", ErrBinding)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: remove stale private supervisor socket: %w", ErrBinding, err)
	}
	return true, syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func (c *Client) privateDaemonPID() (pid int, present bool, resultErr error) {
	if c == nil {
		return 0, false, ErrConfiguration
	}
	if !isPrivateClient(c) {
		return 0, false, nil
	}
	configData, err := readRegular(c.runtimeBinding.ConfigPath, MaxControlBytes)
	if err != nil {
		return 0, false, fmt.Errorf("%w: read private supervisor config for PID: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(configData)
	if err != nil {
		return 0, false, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return 0, false, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return 0, false, err
	}
	stateRoot, _ := privateStateRoot(c.binding.ConfigPath)
	if err = resolved.ValidateIsolation(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil {
		return 0, false, err
	}
	data, err := readRegular(resolved.PIDPath, 128)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("%w: read private supervisor PID: %w", ErrBinding, err)
	}
	text := strings.TrimSpace(string(data))
	pid, err = strconv.Atoi(text)
	if err != nil || pid <= 0 || strconv.Itoa(pid) != text {
		return 0, false, fmt.Errorf("%w: private supervisor PID is invalid", ErrBinding)
	}
	return pid, true, nil
}

// RestartPrivate starts the already-bound private daemon after the caller has
// shut down and drained the old endpoint. It refuses a reachable endpoint and
// publishes the new daemon identity only after readiness succeeds.
func (c *Client) RestartPrivate(ctx context.Context, stateRoot string) error {
	if c == nil {
		return ErrConfiguration
	}
	if !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	if err := c.verifyBinding(); err != nil {
		return err
	}
	if c.runtimeBinding.DaemonExecutable == "" || c.runtimeBinding.DaemonSHA256 == "" {
		return ErrBinding
	}
	if err := c.WaitUnavailable(ctx); err != nil {
		return err
	}
	return c.startPrivate(ctx)
}

// MigratePrivateSupervisorWithLock reconciles a changed private daemon launch
// identity while the caller holds the state-root bootstrap lock. It preserves
// the queue and config, refuses to stop active work, and only clears the
// durable legacy alias after the current daemon is ready and its identity has
// been published.
func (c *Client) MigratePrivateSupervisorWithLock(ctx context.Context, stateRoot string) (resultErr error) {
	if err := validatePrivateMigrationClient(c, stateRoot); err != nil {
		return err
	}
	if err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return fmt.Errorf("%w: recover private identity publication: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	base := filepath.Dir(c.binding.ConfigPath)
	identity, found, pending, err := privateMigrationState(base)
	if err != nil {
		return fmt.Errorf("%w: read private supervisor migration state: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	persistedBinding, bindingFound, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return fmt.Errorf("%w: read private supervisor logical binding: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	current := privateDaemonIdentity{Executable: c.runtimeBinding.DaemonExecutable, SHA256: c.runtimeBinding.DaemonSHA256}
	if err = validatePrivateDaemonIdentity(current); err != nil {
		return fmt.Errorf("%w: current private daemon identity is unavailable: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	currentMigrationIdentity := privateDaemonIdentityForBinding(c.binding)
	if pending != nil {
		if !bindingFound {
			persistedBinding, err = c.resolveMissingMigrationBinding(ctx, stateRoot, identity)
			if err != nil {
				return err
			}
			if err = publishPrivateSupervisorBindingIfAbsent(stateRoot, persistedBinding); err != nil {
				return fmt.Errorf("%w: persist recovered private supervisor binding: %w", ErrPrivateSupervisorMigrationRequired, err)
			}
			bindingFound = true
		}
		return c.resumePrivateMigration(ctx, stateRoot, base, persistedBinding, bindingFound, identity, found, current, currentMigrationIdentity, *pending)
	}
	if !found {
		return c.retryIncompletePrivateBootstrap(ctx, stateRoot)
	}
	if !bindingFound {
		persistedBinding, err = c.resolveMissingMigrationBinding(ctx, stateRoot, identity)
		if err != nil {
			return err
		}
		if err = publishPrivateSupervisorBindingIfAbsent(stateRoot, persistedBinding); err != nil {
			return fmt.Errorf("%w: persist recovered private supervisor binding: %w", ErrPrivateSupervisorMigrationRequired, err)
		}
	}
	return c.reconcilePrivateSupervisor(ctx, stateRoot, persistedBinding, identity, current, currentMigrationIdentity)
}

func validatePrivateMigrationClient(c *Client, stateRoot string) error {
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	return c.verifyBinding()
}

func (c *Client) retryIncompletePrivateBootstrap(ctx context.Context, stateRoot string) error {
	bootstrapPending, bootstrapErr := PrivateBootstrapPending(stateRoot)
	if bootstrapErr != nil {
		return fmt.Errorf("%w: read private bootstrap state: %w", ErrPrivateSupervisorMigrationRequired, bootstrapErr)
	}
	if !bootstrapPending {
		return fmt.Errorf("%w: private supervisor identity is unavailable for retry", ErrPrivateSupervisorMigrationRequired)
	}
	recovery, recoveryErr := PrivateRecoveryBindingPresent(stateRoot)
	if recoveryErr != nil {
		return fmt.Errorf("%w: read private supervisor recovery state: %w", ErrPrivateSupervisorMigrationRequired, recoveryErr)
	}
	if recovery {
		return fmt.Errorf("%w: private supervisor recovery predecessor is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	if startErr := c.StartPrivate(ctx, stateRoot); startErr != nil {
		return fmt.Errorf("%w: retry incomplete private supervisor bootstrap: %w", ErrPrivateSupervisorMigrationRequired, startErr)
	}
	return nil
}

func (c *Client) resolveMissingMigrationBinding(ctx context.Context, stateRoot string, identity privateDaemonIdentity) (task.SupervisorRef, error) {
	if recovery, found, err := ReadPrivateRecoveryBinding(stateRoot); err != nil {
		return task.SupervisorRef{}, fmt.Errorf("%w: read private supervisor recovery binding: %w", ErrPrivateSupervisorMigrationRequired, err)
	} else if found {
		if !privateDaemonIdentityMatchesBinding(identity, recovery) {
			return task.SupervisorRef{}, fmt.Errorf("%w: persisted private supervisor recovery binding does not own the live daemon", ErrPrivateSupervisorMigrationRequired)
		}
		return recovery, nil
	}
	if !privateLogicalIdentityPresent(identity) {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor provenance is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	if !IsPrivateConfig(stateRoot, identity.ConfigPath) || identity.ConfigPath != c.binding.ConfigPath {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor provenance is outside the active state root", ErrPrivateSupervisorMigrationRequired)
	}
	if !privateLogicalIdentityMatchesBinding(identity, c.binding) {
		return task.SupervisorRef{}, fmt.Errorf("%w: persisted private supervisor logical identity differs from the current config", ErrPrivateSupervisorMigrationRequired)
	}
	ready, readyErr := c.availableForBootstrap(ctx)
	if readyErr != nil || !ready {
		return task.SupervisorRef{}, fmt.Errorf("%w: private supervisor logical predecessor binding is unavailable: %w", ErrPrivateSupervisorMigrationRequired, errors.Join(readyErr, ErrBinding))
	}
	// The exact endpoint has answered a validated status request. The persisted
	// daemon identity also carries the prior logical generation, so a changed
	// config cannot be relabeled as the live predecessor.
	return privateSupervisorBindingForIdentity(c.binding, identity), nil
}

func (c *Client) reconcilePrivateSupervisor(ctx context.Context, stateRoot string, persisted task.SupervisorRef, identity, current, currentBinding privateDaemonIdentity) error {
	legacyBinding := privateSupervisorBindingForIdentity(persisted, identity)
	if privateMigrationIdentityMatchesCurrent(identity, current, currentBinding) && task.SameSupervisorIdentity(persisted, c.binding) {
		if startErr := c.StartPrivate(ctx, stateRoot); startErr != nil {
			return fmt.Errorf("%w: verify successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, startErr)
		}
		if clearErr := retirePrivateRecoveryBinding(ctx, stateRoot, c.Binding()); clearErr != nil {
			return fmt.Errorf("%w: clear completed private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
		}
		return nil
	}
	return c.migratePrivateSupervisor(ctx, stateRoot, legacyBinding, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacyBinding),
		Successor:     currentBinding,
	})
}

func privateMigrationState(base string) (identity privateDaemonIdentity, found bool, pending *privateMigrationRecord, resultErr error) {
	identity, found, resultErr = loadPrivateDaemonIdentity(base)
	if resultErr != nil {
		return privateDaemonIdentity{}, false, nil, resultErr
	}
	record, pendingFound, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return privateDaemonIdentity{}, false, nil, err
	}
	if pendingFound {
		pending = &record
	}
	return identity, found, pending, nil
}

func (c *Client) resumePrivateMigration(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if pending.LegacyStopped {
		if privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentBinding) {
			return c.completePendingPrivateMigration(ctx, stateRoot, base, pending)
		}
		// Once shutdown is durable, finish the recorded generation before
		// considering another installation. Never rewrite a launched successor.
		return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, pending)
	}
	if pending.LegacyShutdownDispatched {
		return c.resumeDispatchedPrivateMigrationIfMatching(ctx, stateRoot, base, legacyBinding, bindingFound, identity, found, current, currentBinding, pending)
	}
	if pending.LegacyShutdownRequested {
		return c.resumeRequestedPrivateMigrationIfMatching(ctx, stateRoot, base, legacyBinding, bindingFound, identity, found, current, currentBinding, pending)
	}
	retargeted, err := retargetPrivateMigrationIfNeeded(base, legacyBinding, identity, found, current, currentBinding, pending)
	if err != nil {
		return err
	}
	pending = retargeted
	if err := validatePendingPrivateMigration(pending, legacyBinding, bindingFound, c.binding, current, currentBinding); err != nil {
		return err
	}
	if found && bindingFound && privateMigrationIdentityMatchesCurrent(identity, current, currentBinding) && task.SameSupervisorIdentity(legacyBinding, c.binding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, pending)
	}
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	return c.migratePrivateSupervisor(ctx, stateRoot, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy), pending)
}

func retargetPrivateMigrationIfNeeded(base string, legacyBinding task.SupervisorRef, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) (privateMigrationRecord, error) {
	if !found || !privateDaemonIdentityMatchesBinding(identity, legacyBinding) || privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentBinding) {
		return pending, nil
	}
	// A migration can remain blocked while the installed CLI is upgraded
	// again. The legacy endpoint is still the persisted identity, so the
	// pending successor may be retargeted under the bootstrap lock. This is a
	// deliberate journal replacement; the create-once publisher remains
	// strict for every unrelated record.
	retargeted := pending
	retargeted.Successor = currentBinding
	if err := replacePrivateMigrationRecord(base, pending, retargeted); err != nil {
		return pending, fmt.Errorf("%w: retarget private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return retargeted, nil
}

func (c *Client) resumeDispatchedPrivateMigrationIfMatching(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: dispatched private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	return c.resumeDispatchedPrivateMigration(ctx, stateRoot, base, legacyBinding, bindingFound, current, currentBinding, pending)
}

func (c *Client) resumeRequestedPrivateMigrationIfMatching(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	if !found || !privateDaemonIdentityMatchesBinding(identity, privateSupervisorBindingForIdentity(legacyBinding, pending.Legacy)) {
		return fmt.Errorf("%w: requested private migration journal does not match the persisted daemon identity", ErrPrivateSupervisorMigrationRequired)
	}
	legacy := privateMigrationPIDObserver(legacyBinding, pending.Legacy, c.options)
	updated, _, err := dispatchPrivateMigrationShutdown(ctx, base, legacy, pending)
	if err != nil {
		return err
	}
	if privateMigrationIdentityMatchesCurrent(updated.Successor, current, currentBinding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, updated)
	}
	return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, updated)
}

func (c *Client) resumeDispatchedPrivateMigration(ctx context.Context, stateRoot, base string, legacyBinding task.SupervisorRef, bindingFound bool, current privateDaemonIdentity, currentBinding privateDaemonIdentity, pending privateMigrationRecord) error {
	stopped, err := observeDispatchedPrivateMigrationShutdown(ctx, base, legacyBinding, pending, c.options)
	if err != nil {
		return err
	}
	if privateMigrationIdentityMatchesCurrent(stopped.Successor, current, currentBinding) {
		return c.completePendingPrivateMigration(ctx, stateRoot, base, stopped)
	}
	return c.finishRecordedPrivateSuccessor(ctx, stateRoot, base, legacyBinding, bindingFound, stopped)
}

func observeDispatchedPrivateMigrationShutdown(ctx context.Context, base string, legacyBinding task.SupervisorRef, pending privateMigrationRecord, options Options) (privateMigrationRecord, error) {
	if !pending.LegacyShutdownDispatched {
		return pending, fmt.Errorf("%w: private migration shutdown phase is not dispatched", ErrPrivateSupervisorMigrationRequired)
	}
	if pending.LegacyPID <= 0 {
		return pending, fmt.Errorf("%w: dispatched private migration journal lacks a predecessor PID", ErrPrivateSupervisorMigrationRequired)
	}
	if pending.LegacyStopped {
		return pending, nil
	}
	observer := privateMigrationPIDObserver(legacyBinding, pending.Legacy, options)
	process, err := privateMigrationProcess(pending)
	if err != nil {
		return pending, err
	}
	processUnavailable, err := observer.privateProcessUnavailable(process)
	if err != nil {
		return pending, fmt.Errorf("%w: inspect dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processUnavailable {
		reachable, reachErr := observer.Reachable(ctx)
		if reachErr != nil {
			return pending, fmt.Errorf("%w: inspect dispatched predecessor endpoint: %w", ErrPrivateSupervisorMigrationRequired, reachErr)
		}
		if reachable {
			// Bootstrap serialization outlives every pending command owner. A
			// reachable exact predecessor here proves the prior shutdown command
			// completed without stopping it, so retry the idempotent request.
			retryable := pending
			retryable.LegacyShutdownDispatched = false
			if err := replacePrivateMigrationRecord(base, pending, retryable); err != nil {
				return pending, fmt.Errorf("%w: restore retryable predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
			}
			stopped, _, retryErr := dispatchPrivateMigrationShutdown(ctx, base, observer, retryable)
			return stopped, retryErr
		}
	}
	if err := observer.waitUnavailableWithProcess(ctx, process); err != nil {
		return pending, fmt.Errorf("%w: wait for dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	stopped := pending
	stopped.LegacyStopped = true
	if err := replacePrivateMigrationRecord(base, pending, stopped); err != nil {
		return pending, fmt.Errorf("%w: record dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return stopped, nil
}

func privateMigrationPIDObserver(binding task.SupervisorRef, identity privateDaemonIdentity, options Options) *Client {
	runtime := privateSupervisorBindingForIdentity(binding, identity)
	return &Client{binding: runtime, runtimeBinding: runtime, options: options}
}

func (c *Client) finishRecordedPrivateSuccessor(ctx context.Context, stateRoot, base string, persisted task.SupervisorRef, bindingFound bool, pending privateMigrationRecord) error {
	if !privateLogicalIdentityPresent(pending.Successor) {
		return fmt.Errorf("%w: stopped migration lacks a logical successor binding", ErrPrivateSupervisorMigrationRequired)
	}
	successor := &Client{
		binding:        privateSupervisorBindingForIdentity(c.binding, pending.Successor),
		runtimeBinding: privateSupervisorBindingForIdentity(c.runtimeBinding, pending.Successor),
		options:        c.options,
	}
	identity := privateDaemonIdentity{Executable: pending.Successor.Executable, SHA256: pending.Successor.SHA256}
	if err := validatePendingPrivateMigration(pending, persisted, bindingFound, successor.binding, identity, pending.Successor); err != nil {
		return err
	}
	if err := successor.verifyBinding(); err != nil {
		return fmt.Errorf("%w: recorded successor configuration changed: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if err := successor.completePendingPrivateMigration(ctx, stateRoot, base, pending); err != nil {
		return err
	}
	return c.MigratePrivateSupervisorWithLock(ctx, stateRoot)
}

func validatePendingPrivateMigration(pending privateMigrationRecord, legacyBinding task.SupervisorRef, bindingFound bool, currentBinding task.SupervisorRef, current, currentRef privateDaemonIdentity) error {
	if !privateMigrationIdentityMatchesCurrent(pending.Successor, current, currentRef) {
		return fmt.Errorf("%w: pending migration targets a different daemon installation", ErrPrivateSupervisorMigrationRequired)
	}
	if !privateLogicalIdentityPresent(pending.Successor) && (!bindingFound || !task.SameSupervisorIdentity(legacyBinding, currentBinding)) {
		return fmt.Errorf("%w: legacy migration journal lacks a verified logical successor binding", ErrPrivateSupervisorMigrationRequired)
	}
	if privateLogicalIdentityPresent(pending.Legacy) && !privateDaemonIdentityMatchesBinding(pending.Legacy, legacyBinding) &&
		(!pending.LegacyStopped || !privateDaemonIdentityMatchesBinding(pending.Successor, legacyBinding)) {
		return fmt.Errorf("%w: pending migration has a different logical predecessor binding", ErrPrivateSupervisorMigrationRequired)
	}
	return nil
}

func (c *Client) completePendingPrivateMigration(ctx context.Context, stateRoot, base string, pending privateMigrationRecord) error {
	ready, err := c.availableForBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("%w: verify successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !ready {
		if startErr := c.startPrivate(ctx); startErr != nil {
			return fmt.Errorf("%w: restart successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, startErr)
		}
	}
	if err := publishPrivateDaemonIdentity(base, c.Binding()); err != nil {
		return fmt.Errorf("%w: publish successor before clearing private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if err := appendPrivateMigrationHistory(base, pending); err != nil {
		return fmt.Errorf("%w: record completed private supervisor migration: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if clearErr := clearPrivateMigrationRecord(base, pending); clearErr != nil {
		return fmt.Errorf("%w: clear completed private migration journal: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
	}
	if clearErr := retirePrivateRecoveryBinding(ctx, stateRoot, c.Binding()); clearErr != nil {
		return fmt.Errorf("%w: clear completed private supervisor recovery: %w", ErrPrivateSupervisorMigrationRequired, clearErr)
	}
	return nil
}

func (c *Client) migratePrivateSupervisor(ctx context.Context, stateRoot string, legacy task.SupervisorRef, migration privateMigrationRecord) (resultErr error) {
	base := filepath.Dir(c.binding.ConfigPath)
	if err := publishPrivateMigrationState(base, migration); err != nil {
		return fmt.Errorf("%w: prepare private migration journal: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	oldSupervisor, recoveryOwned, err := RecoverSavedWithPair(ctx, legacy, c.runtimeBinding.ClientExecutable, c.runtimeBinding.DaemonExecutable, c.options)
	if err != nil {
		return fmt.Errorf("%w: recover previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	preserveLegacy := true
	shutdownOwned := false
	shutdownComplete := false
	queueSafe := false
	defer func() {
		if preserveLegacy {
			resultErr = errors.Join(resultErr, oldSupervisor.PersistPrivateRecoveryBinding(stateRoot))
		}
		if recoveryOwned && queueSafe && !shutdownComplete && !shutdownOwned && !errors.Is(resultErr, ErrPrivateSupervisorMigrationBusy) {
			resultErr = errors.Join(resultErr, oldSupervisor.StopRecovered(ctx, c.options.ObservationTimeout, resultErr))
		}
		resultErr = errors.Join(resultErr, oldSupervisor.CloseAfterPending(resultErr))
	}()
	shutdownOwned, shutdownComplete, queueSafe, err = c.runPrivateMigration(ctx, stateRoot, base, oldSupervisor, &migration)
	if err != nil {
		return err
	}
	preserveLegacy = false
	return nil
}

func (c *Client) runPrivateMigration(ctx context.Context, stateRoot, base string, oldSupervisor *Client, migration *privateMigrationRecord) (shutdownOwned, shutdownComplete, queueSafe bool, resultErr error) {
	if migration == nil {
		return false, false, false, ErrConfiguration
	}
	if err := c.authorizePrivateMigrationQueue(ctx, stateRoot, oldSupervisor); err != nil {
		return false, false, false, err
	}
	queueSafe = true
	if err := oldSupervisor.PersistPrivateRecoveryBinding(stateRoot); err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: prepare private supervisor migration: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	oldProcess, processPresent, err := oldSupervisor.privateDaemonProcessIdentity()
	if err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: capture previous private supervisor process identity: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processPresent {
		return false, false, queueSafe, fmt.Errorf("%w: previous private supervisor process identity is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	initial := *migration
	initial.LegacyPID = oldProcess.PID
	initial.LegacyProcessStart = oldProcess.StartToken
	initial.LegacyShutdownRequested = true
	if err = replacePrivateMigrationRecord(base, *migration, initial); err != nil {
		return false, false, queueSafe, fmt.Errorf("%w: record previous private supervisor shutdown request: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	*migration = initial
	// The journal now owns the shutdown request. A crash before Start may safely
	// retry the same idempotent control request against this exact process
	// incarnation; a successful Start is recorded separately before observation.
	shutdownOwned = true
	stopped, _, err := dispatchPrivateMigrationShutdown(ctx, base, oldSupervisor, *migration)
	*migration = stopped
	if err != nil {
		return shutdownOwned, false, queueSafe, err
	}
	shutdownComplete = true
	if err := c.completePendingPrivateMigration(ctx, stateRoot, base, *migration); err != nil {
		return shutdownOwned, shutdownComplete, queueSafe, err
	}
	return shutdownOwned, shutdownComplete, queueSafe, nil
}

func dispatchPrivateMigrationShutdown(ctx context.Context, base string, legacy *Client, pending privateMigrationRecord) (privateMigrationRecord, bool, error) {
	if legacy == nil || !pending.LegacyShutdownRequested || pending.LegacyShutdownDispatched || pending.LegacyStopped {
		return pending, false, fmt.Errorf("%w: private migration shutdown request is invalid", ErrPrivateSupervisorMigrationRequired)
	}
	process, err := privateMigrationProcess(pending)
	if err != nil {
		return pending, false, err
	}
	processUnavailable, err := legacy.privateProcessUnavailable(process)
	if err != nil {
		return pending, false, fmt.Errorf("%w: inspect requested predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if !processUnavailable {
		var dispatched bool
		pending, dispatched, err = requestPrivateMigrationShutdown(ctx, base, legacy, pending)
		if err != nil {
			return pending, dispatched, err
		}
	}
	if err := legacy.waitUnavailableWithProcess(ctx, process); err != nil {
		return pending, pending.LegacyShutdownDispatched, fmt.Errorf("%w: wait for previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	stopped := pending
	stopped.LegacyStopped = true
	if err := replacePrivateMigrationRecord(base, pending, stopped); err != nil {
		return pending, pending.LegacyShutdownDispatched, fmt.Errorf("%w: record previous private supervisor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return stopped, stopped.LegacyShutdownDispatched, nil
}

func requestPrivateMigrationShutdown(ctx context.Context, base string, legacy *Client, pending privateMigrationRecord) (privateMigrationRecord, bool, error) {
	dispatched, shutdownErr := legacy.ShutdownDispatched(ctx)
	if dispatched {
		updated, err := recordPrivateMigrationShutdownDispatched(base, pending)
		if err != nil {
			return pending, true, err
		}
		pending = updated
	}
	if shutdownErr != nil {
		return restoreRetryablePrivateMigrationShutdown(base, pending, dispatched, shutdownErr)
	}
	if !dispatched {
		return pending, false, fmt.Errorf("%w: previous private supervisor shutdown was not dispatched", ErrPrivateSupervisorMigrationRequired)
	}
	return pending, true, nil
}

func recordPrivateMigrationShutdownDispatched(base string, pending privateMigrationRecord) (privateMigrationRecord, error) {
	updated := pending
	updated.LegacyShutdownDispatched = true
	if err := replacePrivateMigrationRecord(base, pending, updated); err != nil {
		return pending, fmt.Errorf("%w: record dispatched predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	return updated, nil
}

func restoreRetryablePrivateMigrationShutdown(base string, pending privateMigrationRecord, dispatched bool, shutdownErr error) (privateMigrationRecord, bool, error) {
	if !dispatched || pendingFrom(shutdownErr) != nil {
		return pending, dispatched, fmt.Errorf("%w: stop previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, shutdownErr)
	}
	retryable := pending
	retryable.LegacyShutdownDispatched = false
	if err := replacePrivateMigrationRecord(base, pending, retryable); err != nil {
		return pending, true, errors.Join(
			fmt.Errorf("%w: restore retryable predecessor shutdown: %w", ErrPrivateSupervisorMigrationRequired, err),
			shutdownErr,
		)
	}
	return retryable, true, fmt.Errorf("%w: stop previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, shutdownErr)
}

func (c *Client) authorizePrivateMigrationQueue(ctx context.Context, stateRoot string, oldSupervisor *Client) error {
	if c == nil || oldSupervisor == nil {
		return ErrConfiguration
	}
	if c.options.PrivateMigrationAuthorizer == nil {
		return fmt.Errorf("%w: private migration policy is not configured", ErrMigrationAuthorization)
	}
	snapshot, err := oldSupervisor.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w: inspect previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, err)
	}
	if err = c.options.PrivateMigrationAuthorizer(ctx, stateRoot, oldSupervisor.Binding(), append([]Job(nil), snapshot.Jobs...)); err != nil {
		if errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
			if preserveErr := oldSupervisor.PersistPrivateRecoveryBinding(stateRoot); preserveErr != nil {
				return errors.Join(ErrPrivateSupervisorMigrationBusy,
					fmt.Errorf("%w: preserve previous private supervisor: %w", ErrPrivateSupervisorMigrationRequired, preserveErr))
			}
		}
		return err
	}
	return nil
}

// PrivateDaemonIdentityDrift reports whether the launch provenance persisted
// by a private state root differs from the daemon executable that the current
// installation would launch. It is read-only; migration remains the only
// operation allowed to stop a reachable supervisor or replace its record.
func PrivateDaemonIdentityDrift(stateRoot, daemonExecutable string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, ErrConfiguration
	}
	resolved, err := canonicalExecutable(daemonExecutable)
	if err != nil {
		return false, err
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		return false, err
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return identity.Executable != resolved || identity.SHA256 != digest, nil
}

// PrivateSupervisorLogicalDrift reports whether the persisted private
// supervisor configuration or endpoint differs from the binding currently
// resolved by the installation. It does not compare executable provenance;
// that is the separate daemon identity drift handled by migration.
func PrivateSupervisorLogicalDrift(stateRoot string, binding task.SupervisorRef) (bool, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return false, ErrConfiguration
	}
	known, matches, err := privateSupervisorBindingStatus(stateRoot, binding)
	if err != nil {
		return false, err
	}
	return known && !matches, nil
}

// PrivateDaemonIdentityPresent reports whether the state root has durable
// launch provenance for a previously started private supervisor. Callers use
// this before starting a replacement when no logical inspection journal can
// identify the predecessor endpoint; an unreachable endpoint plus a saved
// identity is ambiguous and must fail closed.
func PrivateDaemonIdentityPresent(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, ErrConfiguration
	}
	_, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	return found, err
}

// PrivateSupervisorMigrationPending reports whether a validated explicit
// migration journal is present for the state root. Callers use this only to
// select the crash-recovery path before applying endpoint reachability guards;
// MigratePrivateSupervisorWithLock remains the authority that resumes it.
func PrivateSupervisorMigrationPending(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, ErrConfiguration
	}
	_, found, err := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory))
	return found, err
}

// PrivateSupervisorMigrationComplete reports whether the current logical
// binding has been durably published and no bootstrap, publication, migration,
// or predecessor recovery remains. Read-only reuse and exact terminal-row
// maintenance share this predicate; neither may bypass an unfinished transition.
func PrivateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef) (bool, error) {
	return privateSupervisorMigrationComplete(stateRoot, binding, false)
}

// privateSupervisorMigrationComplete is the one completion predicate for a
// published generation. Alias retirement is the only caller allowed to ignore
// the recovery-alias blocker, and it still requires the completed binding,
// daemon identity, and every other transition record to be settled.
func privateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef, allowRecoveryAlias bool) (bool, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return false, ErrConfiguration
	}
	pending, err := PrivateSupervisorMigrationPending(stateRoot)
	if err != nil || pending {
		return false, err
	}
	if !allowRecoveryAlias {
		recovery, recoveryErr := PrivateRecoveryBindingPresent(stateRoot)
		if recoveryErr != nil || recovery {
			return false, recoveryErr
		}
	}
	bootstrapPending, err := PrivateBootstrapPending(stateRoot)
	if err != nil || bootstrapPending {
		return false, err
	}
	if _, publishing, publicationErr := readPrivateIdentityPublication(stateRoot); publicationErr != nil || publishing {
		return false, publicationErr
	}
	identity, found, identityErr := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if identityErr != nil || !found {
		return false, identityErr
	}
	if !privateDaemonIdentityMatchesBinding(identity, binding) {
		return false, nil
	}
	known, matches, err := privateSupervisorBindingStatus(stateRoot, binding)
	return known && matches, err
}

// RefreshPrivateSupervisorBinding updates only daemon launch provenance from
// the state-root record. The logical config and endpoint identity remain
// unchanged so a continuation can reopen its retained inspection generation.
func RefreshPrivateSupervisorBinding(stateRoot string, binding task.SupervisorRef) (task.SupervisorRef, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return binding, nil
	}
	if err := validatePrivateSupervisorRef(binding); err != nil {
		return task.SupervisorRef{}, err
	}
	activeLegacy, err := PrivateRecoveryBindingMatches(stateRoot, binding)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if activeLegacy {
		return binding, nil
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if !found {
		return binding, nil
	}
	binding.DaemonExecutable = identity.Executable
	binding.DaemonSHA256 = identity.SHA256
	return binding, nil
}

// StartPrivate starts the private daemon only when the bound endpoint is not
// already serving. It is intended for callers that prepared a client before
// migration and now hold the bootstrap lock.
func (c *Client) StartPrivate(ctx context.Context, stateRoot string) error {
	if c == nil {
		return ErrConfiguration
	}
	if !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	if err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return err
	}
	ready, err := c.availableForBootstrap(ctx)
	if err != nil {
		return err
	}
	if err := verifyPrivateSupervisorBinding(stateRoot, c.binding, ready); err != nil {
		return err
	}
	if ready {
		_, daemonFound, identityErr := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
		if identityErr != nil {
			return identityErr
		}
		if !daemonFound {
			if publishErr := publishPrivateDaemonIdentity(filepath.Dir(c.binding.ConfigPath), c.Binding()); publishErr != nil {
				return publishErr
			}
		}
		return clearPrivateBootstrapPending(stateRoot)
	}
	return c.startPrivate(ctx)
}

// PreparePrivateWithLock binds the current private supervisor without starting
// its daemon. The caller must hold WithBootstrapLock for stateRoot; this split
// keeps migration and replacement startup in one serialized transition.
func PreparePrivateWithLock(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	return preparePrivateWithLock(ctx, clientExecutable, daemonExecutable, stateRoot, options, false)
}

// PreparePrivateForMigrationWithLock binds the current private supervisor
// without probing its endpoint. Explicit migration uses this form because a
// changed config may point the successor at a new socket while the legacy
// daemon is still serving the old socket. The caller must hold
// WithBootstrapLock for stateRoot and must inspect the legacy endpoint before
// starting the successor.
func PreparePrivateForMigrationWithLock(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	return preparePrivateWithLock(ctx, clientExecutable, daemonExecutable, stateRoot, options, true)
}

// PreparePublishedPrivateWithLock reopens the exact completed generation
// recorded for stateRoot while the caller holds WithBootstrapLock. It first
// completes an interrupted readiness-gated identity publication, then verifies
// the published client, daemon, config, and resolver provenance without
// consulting staged successor inventory. The bool reports whether a published
// generation exists.
func PreparePublishedPrivateWithLock(ctx context.Context, stateRoot string, options Options) (*Client, bool, error) {
	if err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return nil, false, privatePublishedRecoveryError(err)
	}
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return nil, found, err
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, published)
	if err != nil {
		return nil, true, privatePublishedRecoveryError(err)
	}
	if !complete {
		return nil, true, fmt.Errorf("%w: published private supervisor transition is incomplete", ErrPrivateSupervisorMigrationRequired)
	}
	options, err = optionsForPublishedPrivateBinding(published, options)
	if err != nil {
		return nil, true, privatePublishedRecoveryError(err)
	}
	client, err := NewClient(published, options)
	if err != nil {
		return nil, true, privatePublishedRecoveryError(err)
	}
	if err = verifyPublishedPrivatePair(ctx, client, published); err != nil {
		return nil, true, errors.Join(privatePublishedRecoveryError(err), client.CloseAfterPending(err))
	}
	return client, true, nil
}

func optionsForPublishedPrivateBinding(published task.SupervisorRef, options Options) (Options, error) {
	resolution, present, err := resolutionFromBinding(published)
	if err != nil || !present {
		return options, err
	}
	options.Resolution = &resolution
	options.resolutionPinned = true
	if options.Environment == nil || len(options.Environment) > 0 {
		options.Environment = environmentForResolution(options.Environment, resolution)
	}
	return options, nil
}

func verifyPublishedPrivatePair(ctx context.Context, client *Client, published task.SupervisorRef) error {
	if err := client.verify(ctx); err != nil {
		return err
	}
	if err := attachPrivateDaemon(client, published.DaemonExecutable); err != nil {
		return err
	}
	if client.Binding() != published {
		return ErrBinding
	}
	return nil
}

func privatePublishedRecoveryError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		return err
	}
	return fmt.Errorf("%w: published private supervisor pair cannot be recovered: %w", ErrPrivateSupervisorMigrationRequired, err)
}

func preparePrivateWithLock(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, migrationOnly bool) (client *Client, resultErr error) {
	bootstrapContext, cancel, options, err := privateBootstrapOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
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
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = ensurePrivateBase(base); err != nil {
		return nil, err
	}
	if err = recoverPrivateIdentityPublication(stateRoot); err != nil {
		return nil, err
	}
	return preparePrivateClient(bootstrapContext, stateRoot, options, resolution, clientPath, daemonPath, base, migrationOnly, true)
}

func (c *Client) startPrivate(ctx context.Context) error {
	if err := c.verifyBinding(); err != nil {
		return err
	}
	if c.runtimeBinding.DaemonExecutable == "" || c.runtimeBinding.DaemonSHA256 == "" {
		return ErrBinding
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	base := filepath.Dir(c.binding.ConfigPath)
	if private {
		_, identityFound, err := loadPrivateDaemonIdentity(base)
		if err != nil {
			return err
		}
		if !identityFound {
			if err := markPrivateBootstrapPending(stateRoot); err != nil {
				return err
			}
		}
	}
	if err := c.WaitUnavailable(ctx); err != nil {
		return err
	}
	if err := startDaemon(c.runtimeBinding.DaemonExecutable, c.runtimeBinding.DaemonSHA256, c.binding.ConfigPath, base, c.options); err != nil {
		return err
	}
	if err := waitReady(ctx, c, c.options.ObservationTimeout); err != nil {
		return fmt.Errorf("%w: private pueued did not become ready after migration: %w", ErrBinding, err)
	}
	if err := publishPrivateDaemonIdentity(base, c.Binding()); err != nil {
		return err
	}
	if private {
		return clearPrivateBootstrapPending(stateRoot)
	}
	return nil
}

func optionsForSavedBinding(saved task.SupervisorRef, options Options) (Options, error) {
	resolution, present, err := resolutionFromBinding(saved)
	if err != nil {
		return Options{}, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	if !present {
		return options, nil
	}
	if options.Resolution != nil && !sameEnvironmentResolution(*options.Resolution, resolution) {
		return Options{}, ErrConfiguration
	}
	options.Resolution = &resolution
	options.resolutionPinned = true
	if options.Environment == nil || len(options.Environment) > 0 {
		options.Environment = environmentForResolution(options.Environment, resolution)
	}
	return options, nil
}

func bindPrivateClient(ctx context.Context, clientPath, daemonPath, configPath string, options Options) (*Client, bool, error) {
	return bindPrivateClientWithCleanup(ctx, clientPath, daemonPath, configPath, options, true)
}

func bindPrivateClientWithCleanup(ctx context.Context, clientPath, daemonPath, configPath string, options Options, cleanupSuccessful bool) (*Client, bool, error) {
	client, err := Bind(ctx, clientPath, configPath, options)
	if err != nil {
		return nil, false, err
	}
	if err = attachPrivateDaemon(client, daemonPath); err != nil {
		return nil, false, err
	}
	ready, err := privateClientReadyWithCleanup(ctx, client, cleanupSuccessful)
	if err != nil {
		return nil, false, err
	}
	return client, ready, nil
}

func (c *Client) available(ctx context.Context) (bool, error) {
	return c.availableWithCleanup(ctx, false)
}

func (c *Client) availableForBootstrap(ctx context.Context) (bool, error) {
	return c.availableWithCleanup(ctx, true)
}

func (c *Client) availableWithCleanup(ctx context.Context, cleanupSuccessful bool) (bool, error) {
	_, ready, err := privateClientReadySnapshotWithCleanup(ctx, c, cleanupSuccessful)
	return ready, err
}

func (c *Client) cleanupStatusOverflow(ctx context.Context, err error, allowed, cleaned bool) (bool, error) {
	if !allowed || cleaned || !errors.Is(err, errStatusControlLimit) || !isPrivateClient(c) {
		return false, nil
	}
	if cleanErr := c.cleanSuccessfulTasks(ctx); cleanErr != nil {
		return false, cleanErr
	}
	return true, nil
}

func (c *Client) classifyAvailableFailure(ctx context.Context, result CommandResult, err error) (bool, error) {
	if !result.Started || result.ExitCode == 0 {
		return false, err
	}
	unavailable, probeErr := c.endpointUnavailable(ctx)
	if probeErr != nil {
		return false, errors.Join(err, probeErr)
	}
	if unavailable {
		return false, nil
	}
	return false, errors.Join(ErrBinding, err)
}

func isPrivateClient(c *Client) bool {
	if c == nil {
		return false
	}
	_, private := privateStateRoot(c.binding.ConfigPath)
	return private
}

func (c *Client) endpointUnavailable(ctx context.Context) (bool, error) {
	if c == nil || ctx == nil {
		return false, context.Canceled
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return false, err
	}
	return privateEndpointUnavailable(ctx, path)
}

func privateEndpointSocketPath(endpoint string) (string, error) {
	const prefix = "unix:"
	if !strings.HasPrefix(endpoint, prefix) {
		return "", fmt.Errorf("%w: private supervisor endpoint is not a Unix socket", ErrBinding)
	}
	path := strings.TrimPrefix(endpoint, prefix)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: private supervisor endpoint is invalid", ErrBinding)
	}
	return path, nil
}

func privateEndpointUnavailable(ctx context.Context, path string) (bool, error) {
	unavailable, _, err := privateEndpointProbe(ctx, path)
	return unavailable, err
}

func privateEndpointProbe(ctx context.Context, path string) (unavailable, refused bool, resultErr error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		if closeErr := connection.Close(); closeErr != nil {
			return false, false, errors.Join(ErrBinding, closeErr)
		}
		return false, false, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return true, false, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		// The socket inode still exists. It may belong to a daemon that is
		// starting or shutting down, so only a later probe can prove absence.
		return false, true, nil
	}
	return false, false, errors.Join(ErrBinding, err)
}

func privateClientReadyEndpoint(ctx context.Context, client *Client) (bool, error) {
	if client == nil {
		return false, ErrConfiguration
	}
	path, err := privateEndpointSocketPath(client.runtimeBinding.Endpoint)
	if err != nil {
		return false, err
	}
	unavailable, refused, err := privateEndpointProbe(ctx, path)
	if err != nil {
		return false, err
	}
	return !unavailable && !refused, nil
}

func validatePrivateSupervisorRef(ref task.SupervisorRef) error {
	if err := task.ValidateFreshSupervisorRef(ref); err != nil {
		return err
	}
	if (ref.DaemonExecutable == "") != (ref.DaemonSHA256 == "") {
		return errors.New("incomplete saved private supervisor daemon identity")
	}
	if ref.DaemonExecutable == "" {
		return nil
	}
	if err := task.ValidateSHA256(ref.DaemonSHA256); err != nil {
		return err
	}
	return nil
}

func attachPrivateDaemon(client *Client, daemonPath string) error {
	if client == nil {
		return ErrBinding
	}
	resolved, err := canonicalExecutable(daemonPath)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	client.binding.DaemonExecutable = resolved
	client.binding.DaemonSHA256 = digest
	client.runtimeBinding.DaemonExecutable = resolved
	client.runtimeBinding.DaemonSHA256 = digest
	return nil
}

type privateDaemonIdentity struct {
	Executable           string `json:"executable"`
	SHA256               string `json:"sha256"`
	ConfigPath           string `json:"config_path,omitempty"`
	ConfigDigest         string `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}

type privateMigrationRecord struct {
	SchemaVersion            int                   `json:"schema_version"`
	Legacy                   privateDaemonIdentity `json:"legacy"`
	Successor                privateDaemonIdentity `json:"successor"`
	LegacyPID                int                   `json:"legacy_pid,omitempty"`
	LegacyProcessStart       string                `json:"legacy_process_start,omitempty"`
	LegacyShutdownRequested  bool                  `json:"legacy_shutdown_requested,omitempty"`
	LegacyShutdownDispatched bool                  `json:"legacy_shutdown_dispatched,omitempty"`
	LegacyStopped            bool                  `json:"legacy_stopped,omitempty"`
}

type privateMigrationHistory struct {
	SchemaVersion int                      `json:"schema_version"`
	Migrations    []privateMigrationRecord `json:"migrations"`
}

func loadPrivateDaemonIdentity(base string) (identity privateDaemonIdentity, found bool, resultErr error) {
	return loadPrivateDaemonIdentityRaw(base)
}

func loadPrivateDaemonIdentityRaw(base string) (identity privateDaemonIdentity, found bool, resultErr error) {
	data, err := readRegular(privateDaemonIdentityPath(base), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateDaemonIdentity{}, false, nil
	}
	if err != nil {
		return privateDaemonIdentity{}, false, err
	}
	if err := task.DecodeStrict(data, &identity); err != nil {
		return privateDaemonIdentity{}, false, errors.Join(ErrBinding, err)
	}
	if err := validatePrivateDaemonIdentity(identity); err != nil {
		return privateDaemonIdentity{}, false, err
	}
	return identity, true, nil
}

func validatePrivateDaemonIdentity(identity privateDaemonIdentity) error {
	if err := validatePrivateDaemonProvenance(identity); err != nil {
		return err
	}
	return validatePrivateLogicalIdentity(identity)
}

func validatePrivateDaemonProvenance(identity privateDaemonIdentity) error {
	if !filepath.IsAbs(identity.Executable) || filepath.Clean(identity.Executable) != identity.Executable || strings.ContainsRune(identity.Executable, 0) {
		return ErrBinding
	}
	return task.ValidateSHA256(identity.SHA256)
}

func validatePrivateLogicalIdentity(identity privateDaemonIdentity) error {
	if !privateLogicalIdentityPresent(identity) {
		return nil
	}
	if identity.ConfigPath == "" || identity.Endpoint == "" {
		return ErrBinding
	}
	if !filepath.IsAbs(identity.ConfigPath) || filepath.Clean(identity.ConfigPath) != identity.ConfigPath || strings.ContainsRune(identity.ConfigPath, 0) {
		return ErrBinding
	}
	if err := task.ValidateSHA256(identity.ConfigDigest); err != nil {
		return err
	}
	if err := task.ValidateSHA256(identity.ResolvedConfigSHA256); err != nil {
		return err
	}
	_, err := privateEndpointSocketPath(identity.Endpoint)
	return err
}

type privateSupervisorBindingRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Binding       task.SupervisorRef `json:"binding"`
}

type privateIdentityPublicationRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Binding       task.SupervisorRef `json:"binding"`
}

func privateSupervisorBindingPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateSupervisorBindingRecordName)
}

func privateIdentityPublicationPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateIdentityPublicationRecordName)
}

func privateSchedulingEvidencePath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateSchedulingEvidenceRecordName)
}

// ReadPrivateSchedulingEvidence returns the bounded application-owned
// scheduling evidence bytes for this state root. Pueue owns only the fixed
// durable location and file safety; it does not interpret the record.
func ReadPrivateSchedulingEvidence(stateRoot string) ([]byte, bool, error) {
	if !IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) {
		return nil, false, ErrConfiguration
	}
	data, err := readRegular(privateSchedulingEvidencePath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.Join(ErrBinding, err)
	}
	return data, true, nil
}

// ReplacePrivateSchedulingEvidence atomically stores bounded application-owned
// evidence at the existing scheduling record path. Callers serialize policy
// changes with the private transition and bootstrap locks.
func ReplacePrivateSchedulingEvidence(stateRoot string, data []byte) error {
	if !IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) || len(data) == 0 || int64(len(data)) > MaxControlBytes {
		return ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return replacePrivateRecordAtomic(
		base,
		privateSchedulingEvidencePath(stateRoot),
		".private-scheduling-",
		"private scheduling evidence",
		data,
		false,
	)
}

func loadPrivateSupervisorBinding(stateRoot string) (binding task.SupervisorRef, found bool, resultErr error) {
	return loadPrivateSupervisorBindingRaw(stateRoot)
}

// publishPrivateSupervisorBindingIfAbsent records a verified predecessor
// before migration can stop its daemon. It is intentionally create-once: a
// later generation may replace the logical binding only through the
// readiness-gated identity publication transaction.
func publishPrivateSupervisorBindingIfAbsent(stateRoot string, binding task.SupervisorRef) error {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(binding); err != nil {
		return err
	}
	if existing, found, err := loadPrivateSupervisorBinding(stateRoot); err != nil {
		return err
	} else if found {
		if !task.SameSupervisorIdentity(existing, binding) {
			return fmt.Errorf("%w: private supervisor logical binding changed", ErrBinding)
		}
		return nil
	}
	data, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	if err != nil {
		return fmt.Errorf("%w: marshal private supervisor binding: %w", ErrConfiguration, err)
	}
	return publishPrivateRecord(filepath.Join(stateRoot, privateSupervisorDirectory), privateSupervisorBindingPath(stateRoot), data)
}

// PublishPrivateSupervisorBindingIfAbsent records a validated logical owner
// when an older state root has daemon provenance but no separate binding file.
// It never replaces an existing generation.
func PublishPrivateSupervisorBindingIfAbsent(stateRoot string, binding task.SupervisorRef) error {
	return publishPrivateSupervisorBindingIfAbsent(stateRoot, binding)
}

func restorePrivateSupervisorBindingFromSaved(stateRoot string, binding task.SupervisorRef) error {
	if existing, found, err := loadPrivateSupervisorBinding(stateRoot); err != nil {
		return err
	} else if found {
		if !task.SameSupervisorIdentity(existing, binding) {
			return fmt.Errorf("%w: private supervisor logical binding changed", ErrPrivateSupervisorMigrationRequired)
		}
		return nil
	}
	if _, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil {
		return err
	} else if !found {
		return nil
	}
	// A saved task or inspection record is the validated logical owner of this
	// state-rooted config. Recreate only that missing logical record; daemon
	// provenance remains separate and is reconciled by the normal bootstrap.
	return publishPrivateSupervisorBindingIfAbsent(stateRoot, binding)
}

func loadPrivateSupervisorBindingRaw(stateRoot string) (binding task.SupervisorRef, found bool, resultErr error) {
	data, err := readRegular(privateSupervisorBindingPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return task.SupervisorRef{}, false, nil
	}
	if err != nil {
		return task.SupervisorRef{}, false, errors.Join(ErrBinding, err)
	}
	var record privateSupervisorBindingRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return task.SupervisorRef{}, false, errors.Join(ErrBinding, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return task.SupervisorRef{}, false, fmt.Errorf("%w: unsupported private supervisor binding schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err = task.ValidateFreshSupervisorRef(record.Binding); err != nil {
		return task.SupervisorRef{}, false, errors.Join(ErrBinding, err)
	}
	if !IsPrivateConfig(stateRoot, record.Binding.ConfigPath) {
		return task.SupervisorRef{}, false, fmt.Errorf("%w: private supervisor binding is outside its state root", ErrBinding)
	}
	return record.Binding, true, nil
}

func privateSupervisorBindingStatus(stateRoot string, binding task.SupervisorRef) (known, matches bool, resultErr error) {
	persisted, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return found, false, err
	}
	return true, task.SameSupervisorIdentity(persisted, binding), nil
}

func privateSupervisorBindingForIdentity(current task.SupervisorRef, identity privateDaemonIdentity) task.SupervisorRef {
	legacy := current
	legacy.DaemonExecutable = identity.Executable
	legacy.DaemonSHA256 = identity.SHA256
	if identity.ConfigPath != "" {
		legacy.ConfigPath = identity.ConfigPath
		legacy.ConfigDigest = identity.ConfigDigest
		legacy.ResolvedConfigSHA256 = identity.ResolvedConfigSHA256
		legacy.Endpoint = identity.Endpoint
	}
	return legacy
}

func privateDaemonIdentityForBinding(binding task.SupervisorRef) privateDaemonIdentity {
	return privateDaemonIdentity{
		Executable:           binding.DaemonExecutable,
		SHA256:               binding.DaemonSHA256,
		ConfigPath:           binding.ConfigPath,
		ConfigDigest:         binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256,
		Endpoint:             binding.Endpoint,
	}
}

func samePrivateDaemonIdentity(left, right privateDaemonIdentity) bool {
	return left.Executable == right.Executable && left.SHA256 == right.SHA256
}

func privateLogicalIdentityPresent(identity privateDaemonIdentity) bool {
	return identity.ConfigPath != "" || identity.ConfigDigest != "" || identity.ResolvedConfigSHA256 != "" || identity.Endpoint != ""
}

func privateDaemonIdentityMatchesBinding(identity privateDaemonIdentity, binding task.SupervisorRef) bool {
	if !samePrivateDaemonIdentity(identity, privateDaemonIdentityForBinding(binding)) {
		return false
	}
	if !privateLogicalIdentityPresent(identity) {
		return true
	}
	current := privateDaemonIdentityForBinding(binding)
	return identity.ConfigPath == current.ConfigPath &&
		identity.ConfigDigest == current.ConfigDigest &&
		identity.ResolvedConfigSHA256 == current.ResolvedConfigSHA256 &&
		identity.Endpoint == current.Endpoint
}

func privateMigrationIdentityMatchesCurrent(identity, currentDaemon, currentBinding privateDaemonIdentity) bool {
	if !samePrivateDaemonIdentity(identity, currentDaemon) {
		return false
	}
	if !privateLogicalIdentityPresent(identity) {
		return true
	}
	return identity.ConfigPath == currentBinding.ConfigPath &&
		identity.ConfigDigest == currentBinding.ConfigDigest &&
		identity.ResolvedConfigSHA256 == currentBinding.ResolvedConfigSHA256 &&
		identity.Endpoint == currentBinding.Endpoint
}

func verifyPrivateSupervisorBinding(stateRoot string, binding task.SupervisorRef, ready bool) error {
	known, matches, err := privateSupervisorBindingStatus(stateRoot, binding)
	if err != nil {
		return err
	}
	if known && !matches {
		return fmt.Errorf("%w: private supervisor logical binding changed", ErrPrivateSupervisorMigrationRequired)
	}
	if !known {
		_, daemonFound, daemonErr := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
		if daemonErr != nil {
			return daemonErr
		}
		if daemonFound && !ready {
			return fmt.Errorf("%w: private supervisor logical binding is unavailable", ErrPrivateSupervisorMigrationRequired)
		}
	}
	return nil
}

func privateMigrationRecordPath(base string) string {
	return filepath.Join(base, privateMigrationRecordName)
}

type privateBootstrapRecord struct {
	SchemaVersion int `json:"schema_version"`
}

func privateBootstrapRecordPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, privateBootstrapRecordName)
}

// PrivateBootstrapPending reports whether a private bootstrap has durable
// intent without a published daemon identity. It distinguishes a retryable
// Delegate-owned startup interrupted before publication from an unrelated
// pre-existing private config whose owner cannot be proven.
func PrivateBootstrapPending(stateRoot string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return false, ErrConfiguration
	}
	data, err := readRegular(privateBootstrapRecordPath(stateRoot), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record privateBootstrapRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return false, errors.Join(ErrBinding, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return false, fmt.Errorf("%w: unsupported private bootstrap journal schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	return true, nil
}

func markPrivateBootstrapPending(stateRoot string) error {
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
	if err != nil {
		return fmt.Errorf("%w: marshal private bootstrap journal: %w", ErrConfiguration, err)
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	return publishPrivateRecord(base, privateBootstrapRecordPath(stateRoot), data)
}

func clearPrivateBootstrapPending(stateRoot string) error {
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil || !pending {
		return err
	}
	if err = os.Remove(privateBootstrapRecordPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove private bootstrap journal: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func loadPrivateMigrationRecord(base string) (record privateMigrationRecord, found bool, resultErr error) {
	data, err := readRegular(privateMigrationRecordPath(base), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateMigrationRecord{}, false, nil
	}
	if err != nil {
		return privateMigrationRecord{}, false, errors.Join(ErrBinding, err)
	}
	if err = task.DecodeStrict(data, &record); err != nil {
		return privateMigrationRecord{}, false, errors.Join(ErrBinding, err)
	}
	if record.SchemaVersion != task.SchemaVersion {
		return privateMigrationRecord{}, false, fmt.Errorf("%w: unsupported private migration journal schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err = validatePrivateDaemonIdentity(record.Legacy); err != nil {
		return privateMigrationRecord{}, false, fmt.Errorf("%w: invalid private migration legacy identity: %w", ErrBinding, err)
	}
	if err = validatePrivateDaemonIdentity(record.Successor); err != nil {
		return privateMigrationRecord{}, false, fmt.Errorf("%w: invalid private migration successor identity: %w", ErrBinding, err)
	}
	if record.Legacy == record.Successor {
		return privateMigrationRecord{}, false, fmt.Errorf("%w: private migration journal has identical identities", ErrBinding)
	}
	if err = validatePrivateMigrationProcessState(record); err != nil {
		return privateMigrationRecord{}, false, fmt.Errorf("%w: invalid private migration predecessor state", ErrBinding)
	}
	return record, true, nil
}

func publishPrivateMigrationRecord(base string, legacy, successor privateDaemonIdentity) error {
	if err := validatePrivateDaemonIdentity(legacy); err != nil {
		return fmt.Errorf("%w: invalid private migration legacy identity: %w", ErrConfiguration, err)
	}
	if err := validatePrivateDaemonIdentity(successor); err != nil {
		return fmt.Errorf("%w: invalid private migration successor identity: %w", ErrConfiguration, err)
	}
	if legacy == successor {
		return fmt.Errorf("%w: private migration identities are identical", ErrConfiguration)
	}
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: legacy, Successor: successor}
	return publishPrivateMigrationState(base, record)
}

// publishPrivateMigrationState preserves progress when resuming a journal.
// Other publishers retain their create-once equality checks.
func publishPrivateMigrationState(base string, record privateMigrationRecord) error {
	data, err := marshalPrivateMigrationRecord(record)
	if err != nil {
		return err
	}
	return publishPrivateRecord(base, privateMigrationRecordPath(base), data)
}

func clearPrivateMigrationRecord(base string, expected privateMigrationRecord) error {
	record, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found {
		return err
	}
	if record != expected {
		return fmt.Errorf("%w: private migration journal changed", ErrBinding)
	}
	if err = os.Remove(privateMigrationRecordPath(base)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove private migration journal: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(base)
}

func replacePrivateMigrationRecord(base string, expected, replacement privateMigrationRecord) error {
	current, found, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return err
	}
	if !found || current != expected {
		return fmt.Errorf("%w: private migration journal changed", ErrBinding)
	}
	data, err := marshalPrivateMigrationRecord(replacement)
	if err != nil {
		return err
	}
	return replacePrivateRecord(base, privateMigrationRecordPath(base), data)
}

func privateMigrationHistoryPath(base string) string {
	return filepath.Join(base, privateMigrationHistoryRecordName)
}

func loadPrivateMigrationHistory(base string) (history privateMigrationHistory, found bool, resultErr error) {
	data, err := readRegular(privateMigrationHistoryPath(base), MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return privateMigrationHistory{}, false, nil
	}
	if err != nil {
		return privateMigrationHistory{}, false, errors.Join(ErrBinding, err)
	}
	if err = task.DecodeStrict(data, &history); err != nil {
		return privateMigrationHistory{}, false, errors.Join(ErrBinding, err)
	}
	if history.SchemaVersion != task.SchemaVersion {
		return privateMigrationHistory{}, false, fmt.Errorf("%w: unsupported private migration history schema %d", task.ErrUnsupportedSchema, history.SchemaVersion)
	}
	for _, migration := range history.Migrations {
		if _, err = marshalPrivateMigrationRecord(migration); err != nil {
			return privateMigrationHistory{}, false, err
		}
		if err = validatePrivateMigrationHistoryRecord(migration); err != nil {
			return privateMigrationHistory{}, false, err
		}
	}
	return history, true, nil
}

func appendPrivateMigrationHistory(base string, migration privateMigrationRecord) error {
	if _, err := marshalPrivateMigrationRecord(migration); err != nil {
		return err
	}
	if err := validatePrivateMigrationHistoryRecord(migration); err != nil {
		return err
	}
	history, found, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return err
	}
	if !found {
		history = privateMigrationHistory{SchemaVersion: task.SchemaVersion}
	}
	for _, existing := range history.Migrations {
		if existing == migration {
			return nil
		}
	}
	history.Migrations = append(history.Migrations, migration)
	data, err := task.MarshalCanonical(history)
	if err != nil {
		return fmt.Errorf("%w: marshal private migration history: %w", ErrConfiguration, err)
	}
	return replacePrivateRecord(base, privateMigrationHistoryPath(base), data)
}

func privateLogicalIdentityMatchesBinding(identity privateDaemonIdentity, binding task.SupervisorRef) bool {
	if !privateLogicalIdentityPresent(identity) {
		return false
	}
	current := privateDaemonIdentityForBinding(binding)
	return identity.ConfigPath == current.ConfigPath &&
		identity.ConfigDigest == current.ConfigDigest &&
		identity.ResolvedConfigSHA256 == current.ResolvedConfigSHA256 &&
		identity.Endpoint == current.Endpoint
}

// PrivateSupervisorGenerationMatches reports whether a task or inspection
// receipt belongs to the current private supervisor generation or a published
// predecessor retained in the migration history. Terminal queue rows remain
// auditable after a later logical migration without accepting another root's
// supervisor identity.
func PrivateSupervisorGenerationMatches(stateRoot string, binding task.SupervisorRef) (bool, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return false, ErrConfiguration
	}
	if current, found, err := loadPrivateSupervisorBinding(stateRoot); err != nil {
		return false, err
	} else if found && task.SameSupervisorIdentity(current, binding) {
		return true, nil
	}
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	for _, migration := range history.Migrations {
		if privateLogicalIdentityMatchesBinding(migration.Legacy, binding) || privateLogicalIdentityMatchesBinding(migration.Successor, binding) {
			return true, nil
		}
	}
	return false, nil
}

func marshalPrivateMigrationRecord(record privateMigrationRecord) ([]byte, error) {
	if record.SchemaVersion != task.SchemaVersion {
		return nil, fmt.Errorf("%w: invalid private migration journal schema", ErrConfiguration)
	}
	if err := validatePrivateDaemonIdentity(record.Legacy); err != nil {
		return nil, fmt.Errorf("%w: invalid private migration legacy identity: %w", ErrConfiguration, err)
	}
	if err := validatePrivateDaemonIdentity(record.Successor); err != nil {
		return nil, fmt.Errorf("%w: invalid private migration successor identity: %w", ErrConfiguration, err)
	}
	if record.Legacy == record.Successor {
		return nil, fmt.Errorf("%w: private migration identities are identical", ErrConfiguration)
	}
	if err := validatePrivateMigrationProcessState(record); err != nil {
		return nil, fmt.Errorf("%w: invalid private migration predecessor state", ErrConfiguration)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal replacement private migration journal: %w", ErrConfiguration, err)
	}
	return data, nil
}

func validatePrivateMigrationHistoryRecord(record privateMigrationRecord) error {
	if !record.LegacyStopped || record.LegacyPID <= 0 {
		return fmt.Errorf("%w: private migration history does not prove predecessor shutdown", ErrBinding)
	}
	return nil
}

func validatePrivateMigrationProcessState(record privateMigrationRecord) error {
	if record.LegacyPID < 0 {
		return errors.New("negative predecessor PID")
	}
	if record.LegacyProcessStart != "" && record.LegacyPID <= 0 {
		return errors.New("process start token lacks predecessor PID")
	}
	if record.LegacyShutdownRequested && (record.LegacyPID <= 0 || record.LegacyProcessStart == "") {
		return errors.New("shutdown request lacks predecessor process identity")
	}
	if record.LegacyShutdownDispatched && record.LegacyPID <= 0 {
		return errors.New("dispatched shutdown lacks predecessor PID")
	}
	if record.LegacyStopped && record.LegacyPID <= 0 {
		return errors.New("stopped predecessor lacks PID")
	}
	return nil
}

func privateMigrationProcess(record privateMigrationRecord) (privateProcessIdentity, error) {
	if record.LegacyPID <= 0 || record.LegacyProcessStart == "" {
		return privateProcessIdentity{}, fmt.Errorf("%w: private migration journal lacks a predecessor process identity", ErrPrivateSupervisorMigrationRequired)
	}
	return privateProcessIdentity{PID: record.LegacyPID, StartToken: record.LegacyProcessStart}, nil
}

func replacePrivateRecord(base, destination string, data []byte) error {
	return replacePrivateRecordAtomic(base, destination, ".private-migration-", "private migration journal", data, false)
}

// replacePrivateRecordAtomic stages one existing private record and atomically
// replaces it. The caller supplies the stage prefix and record context so the
// error identifies the record being changed. Binding publication requests a
// directory barrier before rename as well as the common barrier afterward.
func replacePrivateRecordAtomic(base, destination, stagePrefix, recordContext string, data []byte, syncBeforeRename bool) (resultErr error) {
	file, stagePath, err := createPrivateStage(base, stagePrefix)
	if err != nil {
		return fmt.Errorf("%w: stage %s replacement: %w", ErrConfiguration, recordContext, err)
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(fmt.Errorf("%w: write %s: %w", ErrConfiguration, recordContext, writeErr), file.Close())
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(fmt.Errorf("%w: sync %s: %w", ErrConfiguration, recordContext, err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrConfiguration, recordContext, err)
	}
	if syncBeforeRename {
		if err = syncPrivateDirectory(base); err != nil {
			return fmt.Errorf("%w: sync %s directory before replacement: %w", ErrConfiguration, recordContext, err)
		}
	}
	if err = os.Rename(stagePath, destination); err != nil {
		return fmt.Errorf("%w: atomically replace %s: %w", ErrConfiguration, recordContext, err)
	}
	stagePath = ""
	if err = syncPrivateDirectory(base); err != nil {
		return fmt.Errorf("%w: sync %s directory after replacement: %w", ErrConfiguration, recordContext, err)
	}
	return nil
}

func privateDaemonIdentityPath(base string) string {
	return filepath.Join(base, "daemon.identity.json")
}

func recoverPrivateIdentityPublication(stateRoot string) error {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrConfiguration
	}
	pending, found, err := readPrivateIdentityPublication(stateRoot)
	if err != nil || !found {
		return err
	}
	return publishPrivateIdentityPublication(stateRoot, pending)
}

func readPrivateIdentityPublication(stateRoot string) (privateIdentityPublicationRecord, bool, error) {
	var pending privateIdentityPublicationRecord
	pendingPath := privateIdentityPublicationPath(stateRoot)
	data, err := readRegular(pendingPath, MaxControlBytes)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, fmt.Errorf("%w: read private identity publication: %w", ErrBinding, err)
	}
	if err = task.DecodeStrict(data, &pending); err != nil {
		return pending, false, fmt.Errorf("%w: decode private identity publication: %w", ErrBinding, err)
	}
	if pending.SchemaVersion != task.SchemaVersion {
		return pending, false, fmt.Errorf("%w: unsupported private identity publication schema %d", task.ErrUnsupportedSchema, pending.SchemaVersion)
	}
	if !IsPrivateConfig(stateRoot, pending.Binding.ConfigPath) {
		return pending, false, fmt.Errorf("%w: private identity publication is outside its state root", ErrBinding)
	}
	if err = validatePrivateSupervisorRef(pending.Binding); err != nil {
		return pending, false, fmt.Errorf("%w: validate private identity publication: %w", ErrBinding, err)
	}
	identity := privateDaemonIdentityForBinding(pending.Binding)
	if err = validatePrivateDaemonIdentity(identity); err != nil {
		return pending, false, fmt.Errorf("%w: validate private identity publication daemon: %w", ErrBinding, err)
	}
	return pending, true, nil
}

func publishPrivateIdentityPublication(stateRoot string, pending privateIdentityPublicationRecord) error {
	pendingPath := privateIdentityPublicationPath(stateRoot)
	bindingData, err := task.MarshalCanonical(privateSupervisorBindingRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       pending.Binding,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication binding: %w", ErrConfiguration, err)
	}
	daemonData, err := task.MarshalCanonical(privateDaemonIdentityForBinding(pending.Binding))
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication daemon: %w", ErrConfiguration, err)
	}
	if err = replacePrivateSupervisorBindingRecord(stateRoot, bindingData); err != nil {
		return err
	}
	if err = replacePrivateDaemonIdentityRecord(filepath.Join(stateRoot, privateSupervisorDirectory), daemonData); err != nil {
		return err
	}
	if err = os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear private identity publication: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func publishPrivateDaemonIdentity(base string, binding task.SupervisorRef) error {
	identity := privateDaemonIdentityForBinding(binding)
	if err := validatePrivateDaemonIdentity(identity); err != nil {
		return fmt.Errorf("%w: validate private supervisor identity: %w", ErrConfiguration, err)
	}
	if binding.ConfigPath == "" {
		return publishDaemonOnlyIdentity(base, identity)
	}
	if err := task.ValidateFreshSupervisorRef(binding); err != nil {
		return fmt.Errorf("%w: validate private supervisor binding: %w", ErrConfiguration, err)
	}
	stateRoot, private := privateStateRoot(binding.ConfigPath)
	if !private {
		return publishDaemonOnlyIdentity(base, identity)
	}
	data, err := task.MarshalCanonical(identity)
	if err != nil {
		return fmt.Errorf("%w: marshal private daemon identity: %w", ErrConfiguration, err)
	}
	if err = recoverPrivateIdentityPublication(stateRoot); err != nil {
		return err
	}
	bindingData, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	if err != nil {
		return fmt.Errorf("%w: marshal private supervisor binding: %w", ErrConfiguration, err)
	}
	pendingData, err := task.MarshalCanonical(privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       binding,
	})
	if err != nil {
		return fmt.Errorf("%w: marshal private identity publication: %w", ErrConfiguration, err)
	}
	if err = publishPrivateRecord(filepath.Join(stateRoot, privateSupervisorDirectory), privateIdentityPublicationPath(stateRoot), pendingData); err != nil {
		return err
	}
	if err = replacePrivateSupervisorBindingRecord(filepath.Dir(base), bindingData); err != nil {
		return err
	}
	if err = replacePrivateDaemonIdentityRecord(base, data); err != nil {
		return err
	}
	if err = os.Remove(privateIdentityPublicationPath(stateRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: clear private identity publication: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func publishDaemonOnlyIdentity(base string, identity privateDaemonIdentity) error {
	data, err := task.MarshalCanonical(privateDaemonIdentity{Executable: identity.Executable, SHA256: identity.SHA256})
	if err != nil {
		return fmt.Errorf("%w: marshal private daemon identity: %w", ErrConfiguration, err)
	}
	return replacePrivateDaemonIdentityRecord(base, data)
}

func replacePrivateSupervisorBindingRecord(stateRoot string, data []byte) error {
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	destination := privateSupervisorBindingPath(stateRoot)
	return replacePrivateRecordAtomic(base, destination, ".private-binding-", "private supervisor binding", data, true)
}

// replacePrivateDaemonIdentityRecord atomically replaces launch provenance
// after the supervisor it describes has passed readiness. The create-once
// publisher below remains strict for all other private records.
func replacePrivateDaemonIdentityRecord(base string, data []byte) error {
	destination := privateDaemonIdentityPath(base)
	return replacePrivateRecordAtomic(base, destination, ".private-record-", "private daemon identity", data, false)
}

func publishPrivateRecord(base, destination string, data []byte) (resultErr error) {
	file, stagePath, err := createPrivateStage(base, ".private-record-")
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(fmt.Errorf("%w: write private record: %w", ErrConfiguration, writeErr), file.Close())
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(fmt.Errorf("%w: sync private record: %w", ErrConfiguration, err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close private record: %w", ErrConfiguration, err)
	}
	if err = os.Link(stagePath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: publish private record: %w", ErrConfiguration, err)
		}
		existing, readErr := readRegular(destination, MaxControlBytes)
		if readErr != nil {
			return errors.Join(ErrBinding, readErr)
		}
		if !bytes.Equal(existing, data) {
			return ErrBinding
		}
		if removeErr := os.Remove(stagePath); removeErr != nil {
			return fmt.Errorf("%w: discard private record stage: %w", ErrConfiguration, removeErr)
		}
		stagePath = ""
		return syncPrivateDirectory(base)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return err
	}
	if err = os.Remove(stagePath); err != nil {
		return fmt.Errorf("%w: remove private record stage: %w", ErrConfiguration, err)
	}
	stagePath = ""
	return syncPrivateDirectory(base)
}

func privateClientReady(ctx context.Context, client *Client) (bool, error) {
	return privateClientReadyWithCleanup(ctx, client, true)
}

func privateClientReadyWithCleanup(ctx context.Context, client *Client, cleanupSuccessful bool) (bool, error) {
	_, ready, err := privateClientReadySnapshotWithCleanup(ctx, client, cleanupSuccessful)
	return ready, err
}

func privateClientReadySnapshotWithCleanup(ctx context.Context, client *Client, cleanupSuccessful bool) (QueueSnapshot, bool, error) {
	if client == nil {
		return QueueSnapshot{}, false, ErrConfiguration
	}
	cleaned := false
	for {
		result, err := privateReadyAttempt(ctx, client)
		if err != nil {
			if handled, unavailable, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
				if endpointErr != nil {
					return QueueSnapshot{}, false, endpointErr
				}
				return QueueSnapshot{}, !unavailable, nil
			}
			if retry, cleanErr := client.cleanupStatusOverflow(ctx, err, cleanupSuccessful, cleaned); cleanErr != nil {
				return QueueSnapshot{}, false, errors.Join(err, cleanErr)
			} else if retry {
				cleaned = true
				continue
			}
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		if err = client.validateReadyResult(result); err != nil {
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		snapshot, err := snapshotFromResult(result, client.runtimeBinding.ObservedVersion)
		if err != nil {
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		return snapshot, true, nil
	}
}

func privateReadyAttempt(ctx context.Context, client *Client) (CommandResult, error) {
	result, err := client.readyCommand(ctx)
	if pending := pendingFrom(err); pending != nil {
		return awaitReadyPending(ctx, client, pending)
	}
	return result, err
}

// privateClientReadyGroups verifies the private endpoint through Pueue's
// bounded group response. Lifecycle admission needs only daemon liveness and
// group scheduling facts; it must not deserialize every task row while a large
// private queue is active.
func privateClientReadyGroups(ctx context.Context, client *Client) (map[string]Group, bool, error) {
	if client == nil {
		return nil, false, ErrConfiguration
	}
	groups, result, err := client.privateGroups(ctx)
	if err == nil {
		return groups, true, nil
	}
	if pendingFrom(err) != nil {
		return nil, false, err
	}
	if handled, unavailable, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
		if endpointErr != nil {
			return nil, false, endpointErr
		}
		return nil, !unavailable, nil
	}
	return nil, false, errors.Join(ErrBinding, err)
}

func (c *Client) privateGroups(ctx context.Context) (map[string]Group, CommandResult, error) {
	result, err := c.groupAfterVerifiedBinding(ctx)
	if pending := pendingFrom(err); pending != nil {
		result, err = awaitPending(ctx, pending)
	}
	if err != nil {
		return nil, result, err
	}
	groups, err := groupsFromResult(result)
	return groups, result, err
}

func privateReadyEndpointFailure(ctx context.Context, client *Client, result CommandResult, err error) (handled, unavailable bool, resultErr error) {
	// A nonzero liveness command is only an absent endpoint when the exact Unix
	// socket also proves absent. A live endpoint with an incompatible response
	// must never be mistaken for a stopped daemon during migration.
	if !result.Started || result.ExitCode == 0 {
		return false, false, nil
	}
	unavailable, probeErr := client.endpointUnavailable(ctx)
	if probeErr != nil {
		return true, false, errors.Join(err, probeErr)
	}
	if unavailable {
		return true, true, nil
	}
	return true, false, errors.Join(ErrBinding, err)
}

// cleanSuccessfulTasks removes only exact terminal rows authorized by the
// application. It never issues broad clean, including after status overflow.
func (c *Client) cleanSuccessfulTasks(ctx context.Context) error {
	snapshot, ids, err := c.authorizePrivateSuccessfulTasks(ctx)
	if err != nil {
		return err
	}
	return c.removeAuthorizedTerminalRows(ctx, snapshot, ids)
}

// authorizePrivateSuccessfulTasks takes one queue snapshot and asks the
// application-owned policy to validate its terminal rows. The callback is the
// sole owner of Delegate record interpretation; it records independently
// observed worker exits before any caller is allowed to remove a row.
func (c *Client) authorizePrivateSuccessfulTasks(ctx context.Context) (QueueSnapshot, []int64, error) {
	if c == nil {
		return QueueSnapshot{}, nil, ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return QueueSnapshot{}, nil, ErrConfiguration
	}
	if c.options.SuccessfulCleanupAuthorizer == nil {
		return QueueSnapshot{}, nil, ErrCleanupAuthorization
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return QueueSnapshot{}, nil, err
	}
	ids, err := c.options.SuccessfulCleanupAuthorizer(ctx, stateRoot, c.binding, cloneQueueSnapshot(snapshot).Jobs)
	if err != nil {
		return QueueSnapshot{}, nil, err
	}
	return snapshot, ids, nil
}

func (c *Client) removeAuthorizedTerminalRows(ctx context.Context, snapshot QueueSnapshot, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ended := make(map[int64]int, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		if job.State == StateEnded {
			ended[job.ID]++
		}
	}
	args := make([]string, 1, len(ids)+1)
	args[0] = "remove"
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if ended[id] != 1 {
			return ErrCleanupAuthorization
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrCleanupAuthorization
		}
		seen[id] = struct{}{}
		args = append(args, strconv.FormatInt(id, 10))
	}
	// Pueue accepts multiple explicit IDs in one remove command. The caller
	// holds the exclusive private supervisor lock across this snapshot and
	// mutation, so numeric IDs cannot be reused between authorization and the
	// batch removal.
	result, err := c.command(ctx, nil, args...)
	if pending := pendingFrom(err); pending != nil {
		settled, waitErr := awaitPending(ctx, pending)
		result, err = settled, waitErr
	}
	if err = errors.Join(err, result.Err); err != nil {
		return err
	}
	return c.ClearPrivateTerminalEvents(ctx, ids...)
}

// MaintainPrivateQueue opportunistically reaps terminal private-supervisor
// rows that the application has authorized from durable evidence. It returns
// true when this private client can perform or coalesce that cleanup. It never
// waits behind an active lifecycle transition and it never changes admission
// or inspection authority when maintenance cannot run.
func (c *Client) MaintainPrivateQueue(ctx context.Context) bool {
	if c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	finish, _, available := c.beginPrivateMaintenance()
	if finish == nil {
		return available
	}
	c.runPrivateQueueMaintenance(ctx, finish, false)
	return true
}

func (c *Client) runPrivateQueueMaintenance(ctx context.Context, finish func(), waitForLease bool) {
	finishOnReturn := true
	defer func() {
		if finishOnReturn {
			finish()
		}
	}()
	maintenanceContext, cancel := context.WithTimeout(ctx, privateQueueMaintenanceTimeout(c.options.ObservationTimeout))
	defer cancel()
	// Maintenance owns a separate bounded lock scope. Its error must reach the
	// lock owner before it is suppressed, preserving any unfinished command's
	// lock until its sole Wait owner records completion.
	lockMode := unix.LOCK_EX | unix.LOCK_NB
	if waitForLease {
		// Scheduled cleanup already runs outside the admission caller. Waiting
		// here lets a terminal observation release its shared generation lease
		// before cleanup takes the exclusive snapshot/removal lease. A
		// synchronous opportunistic probe still skips a busy generation.
		lockMode = unix.LOCK_EX
	}
	// beginPrivateMaintenance already admitted one outer work token. Use a
	// non-owning view for its nested controls so Close can reject new callers
	// while allowing this exact admitted operation to finish.
	worker := c.backgroundClient()
	err := worker.withPrivateSupervisorLock(maintenanceContext, func(lockedContext context.Context) error {
		return worker.cleanSuccessfulTasks(lockedContext)
	}, func(lockContext context.Context, root string, callback func(context.Context) error) error {
		return withPublishedPrivateLockContext(lockContext, root, "bootstrap.lock", lockMode, callback)
	})
	if err != nil {
		finishOnReturn = !retainPrivateQueueWorkUntilPending(err, finish)
		log.Printf("delegate: terminal supervisor cleanup deferred: %v", err)
	}
}

func retainPrivateQueueWorkUntilPending(err error, finish func()) bool {
	pending := pendingHandles(err)
	if len(pending) == 0 {
		return false
	}
	// The work token was admitted before Close could begin. Transfer it to the
	// sole pending-command waiter so client-owned resources outlive every
	// control process started by this background operation.
	go func() {
		for _, handle := range pending {
			<-handle.Done()
		}
		finish()
	}()
	return true
}

func privateQueueMaintenanceTimeout(observationTimeout time.Duration) time.Duration {
	if observationTimeout <= 0 || observationTimeout > DefaultObservationTimeout {
		observationTimeout = DefaultObservationTimeout
	}
	return observationTimeout * privateQueueMaintenanceObservationPhases
}

// SchedulePrivateQueueMaintenance admits one bounded maintenance attempt under
// the client's lifecycle owner. Close waits for admitted work, so maintenance
// cannot outlive the state and configuration resources used by the client.
func (c *Client) SchedulePrivateQueueMaintenance(ctx context.Context) bool {
	if ctx == nil || c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	finish, _, available := c.beginPrivateMaintenance()
	if finish == nil {
		return available
	}
	maintenanceContext := context.WithoutCancel(ctx)
	go c.runPrivateQueueMaintenance(maintenanceContext, finish, true)
	return true
}

func reapReadyPending(ctx context.Context, pending *Pending) (CommandResult, error) {
	return awaitPending(ctx, pending)
}

func awaitReadyPending(ctx context.Context, client *Client, pending *Pending) (CommandResult, error) {
	for pending != nil {
		result, contextErr := reapReadyPending(ctx, pending)
		if contextErr != nil && pendingFrom(contextErr) != nil {
			return result, contextErr
		}
		if !pending.versionProbe() {
			return classifyReadyStatus(result, result.Err)
		}
		if _, versionErr := validateVersionResult(result); versionErr != nil {
			return CommandResult{}, versionErr
		}
		status, statusErr := client.statusAfterVerifiedVersion(ctx)
		if statusErr == nil {
			return status, nil
		}
		if next := pendingFrom(statusErr); next != nil {
			pending = next
			continue
		}
		return status, statusErr
	}
	return CommandResult{}, nil
}

func classifyReadyProcess(result CommandResult) (error, bool) {
	if result.Err == nil {
		return nil, false
	}
	err := errors.Join(ErrBinding, result.Err)
	return err, result.Started && result.ExitCode != 0
}

func processReadyPending(ctx context.Context, client *Client, lastErr error, pending *Pending) (error, bool) {
	result, contextErr := awaitReadyPending(ctx, client, pending)
	if contextErr != nil {
		return errors.Join(lastErr, contextErr), true
	}
	if processErr, retry := classifyReadyProcess(result); processErr != nil {
		if retry {
			return processErr, false
		}
		return processErr, true
	}
	return client.validateReadyResult(result), true
}

func privateResolution(options Options) (ResolutionContext, error) {
	if options.resolutionPinned && options.Resolution != nil {
		return *options.Resolution, nil
	}
	current, err := resolutionContext(environmentForCommand(options.Environment))
	if err != nil {
		return ResolutionContext{}, err
	}
	if options.Resolution == nil {
		return current, nil
	}
	resolution := *options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match private supervisor environment", ErrConfiguration)
	}
	return resolution, nil
}

func ensurePrivateConfig(stateRoot string, resolution ResolutionContext) (configPath, base string, resultErr error) {
	base = filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return "", "", err
	}
	if err := ensurePrivateLockFile(base, privateTaskObservationLockName); err != nil {
		return "", "", err
	}
	configPath = filepath.Join(base, "pueue.yml")
	if err := rejectSymlinkComponents(configPath); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(configPath); err == nil {
		if validateErr := validatePrivateConfig(configPath, base, resolution); validateErr != nil {
			return "", "", validateErr
		}
		return configPath, base, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w: inspect private supervisor config: %w", ErrConfiguration, err)
	}

	// Record bootstrap intent before exposing the config. If the process dies
	// after this point, the next private bind can distinguish its own partial
	// bootstrap from an unowned config-only state root.
	if err := markPrivateBootstrapPending(stateRoot); err != nil {
		return "", "", err
	}
	if err := createPrivateConfig(configPath, base, privateConfigYAML(base)); err != nil {
		return "", "", err
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func createPrivateConfig(configPath, base, data string) (resultErr error) {
	file, stagePath, err := createPrivateConfigStage(base)
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()

	written, writeErr := file.WriteString(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(
			fmt.Errorf("%w: write staged private supervisor config: %w", ErrConfiguration, writeErr),
			file.Close(),
		)
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(
			fmt.Errorf("%w: sync staged private supervisor config: %w", ErrConfiguration, err),
			file.Close(),
		)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close staged private supervisor config: %w", ErrConfiguration, err)
	}

	if err = os.Link(stagePath, configPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: publish private supervisor config: %w", ErrConfiguration, err)
		}
		if removeErr := os.Remove(stagePath); removeErr != nil {
			return errors.Join(
				fmt.Errorf("%w: discard competing private supervisor config: %w", ErrConfiguration, removeErr),
				err,
			)
		}
		stagePath = ""
		return syncPrivateDirectory(base)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return err
	}
	if err = os.Remove(stagePath); err != nil {
		return fmt.Errorf("%w: remove staged private supervisor config: %w", ErrConfiguration, err)
	}
	stagePath = ""
	return syncPrivateDirectory(base)
}

func createPrivateConfigStage(base string) (*os.File, string, error) {
	return createPrivateStage(base, ".pueue.yml-")
}

func createPrivateStage(base, prefix string) (*os.File, string, error) {
	for range 4 {
		id, err := task.NewRandomID()
		if err != nil {
			return nil, "", fmt.Errorf("%w: create staged private supervisor config: %w", ErrConfiguration, err)
		}
		stagePath := filepath.Join(base, prefix+id)
		file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("%w: create staged private supervisor config: %w", ErrConfiguration, err)
		}
		return file, stagePath, nil
	}
	return nil, "", fmt.Errorf("%w: create unique staged private supervisor config: %w", ErrConfiguration, os.ErrExist)
}

func ensurePrivateBase(base string) error {
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	missing, err := missingPrivateDirectories(base)
	if err != nil {
		return err
	}
	for _, directory := range missing {
		if err := createPrivateDirectory(directory); err != nil {
			return err
		}
	}
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	// Auxiliary locks belong to their own mutation paths. A shared probe must
	// never create them: the transition election and task-status observation
	// lock are provisioned after lifecycle ownership is elected, and terminal
	// event state only when a ticket or event is written.
	if err := ensurePrivateLockFile(base, "bootstrap.lock"); err != nil {
		return err
	}
	return nil
}

func ensurePrivateLockFile(base, name string) (resultErr error) {
	path := filepath.Join(base, name)
	for {
		present, err := privateLockFilePresent(base, name)
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			continue
		}
		if createErr != nil {
			return fmt.Errorf("%w: create private supervisor lock: %w", ErrConfiguration, createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("%w: close private supervisor lock: %w", ErrConfiguration, closeErr), os.Remove(path))
		}
		return syncPrivateDirectory(base)
	}
}

func privateLockFilePresent(base, name string) (bool, error) {
	if name == "" || filepath.Base(name) != name {
		return false, ErrConfiguration
	}
	path := filepath.Join(base, name)
	if err := rejectSymlinkComponents(path); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect private supervisor lock: %w", ErrConfiguration, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("%w: private supervisor lock is not a protected regular file", ErrConfiguration)
	}
	return true, nil
}

func missingPrivateDirectories(base string) ([]string, error) {
	var missing []string
	current := base
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("%w: private supervisor path is not a directory", ErrConfiguration)
			}
			return missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: inspect private supervisor directory: %w", ErrConfiguration, err)
		}
		missing = append([]string{current}, missing...)
		parent := filepath.Dir(current)
		if parent == current {
			return missing, nil
		}
		current = parent
	}
}

func createPrivateDirectory(directory string) error {
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: create private supervisor directory: %w", ErrConfiguration, err)
	}
	if err := privateBarrierDir(directory); err != nil {
		return fmt.Errorf("%w: barrier private supervisor directory: %w", ErrConfiguration, err)
	}
	if err := privateBarrierDir(filepath.Dir(directory)); err != nil {
		return fmt.Errorf("%w: barrier private supervisor parent: %w", ErrConfiguration, err)
	}
	return nil
}

func syncPrivateDirectory(path string) error {
	if err := privateBarrierDir(path); err != nil {
		return fmt.Errorf("%w: sync private supervisor directory: %w", ErrConfiguration, err)
	}
	return nil
}

func readPrivateConfig(configPath, base string, resolution ResolutionContext) (ResolvedConfig, error) {
	data, err := readRegular(configPath, MaxControlBytes)
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("%w: read private supervisor config: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return ResolvedConfig{}, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return ResolvedConfig{}, err
	}
	if err = resolved.ValidateIsolation(base); err != nil {
		return ResolvedConfig{}, err
	}
	return resolved, nil
}

func validatePrivateConfig(configPath, base string, resolution ResolutionContext) error {
	resolved, err := readPrivateConfig(configPath, base, resolution)
	if err != nil {
		return err
	}
	for _, directory := range []string{resolved.PueueDirectory, resolved.RuntimeDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("%w: create private supervisor data: %w", ErrConfiguration, err)
		}
		if err := rejectSymlinkComponents(directory); err != nil {
			return err
		}
	}
	return nil
}

func privateConfigYAML(base string) string {
	quote := strconv.Quote
	data := filepath.Join(base, "data")
	runtimeDirectory := filepath.Join(base, "run")
	return strings.Join([]string{
		"client:",
		"  show_confirmation_questions: false",
		"shared:",
		"  pueue_directory: " + quote(data),
		"  runtime_directory: " + quote(runtimeDirectory),
		"  alias_file: " + quote(filepath.Join(base, "aliases.yml")),
		"  use_unix_socket: true",
		"  unix_socket_path: " + quote(filepath.Join(runtimeDirectory, "pueue.sock")),
		"  unix_socket_permissions: 448",
		"  pid_path: " + quote(filepath.Join(runtimeDirectory, "pueue.pid")),
		"  shared_secret_path: " + quote(filepath.Join(data, "shared_secret")),
		"daemon:",
		"  callback: " + quote(privateTerminalEventCallback(base)),
		"  shell_command:",
		"    - \"/bin/sh\"",
		"    - \"-c\"",
		"    - \"{{ pueue_command_string }}\"",
		"",
	}, "\n")
}

func startDaemon(executable, expectedSHA256, configPath, base string, options Options) error {
	digest, err := hashExecutable(executable)
	if err != nil {
		return fmt.Errorf("%w: verify private pueued identity before start: %w", ErrBinding, err)
	}
	if digest != expectedSHA256 {
		return fmt.Errorf("%w: private pueued identity changed before start", ErrBinding)
	}
	cmd := exec.Command(executable, "-c", configPath)
	cmd.Dir = base
	cmd.Env = environmentForCommand(options.Environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = configurePrivateDaemonCommand(cmd); err != nil {
		return fmt.Errorf("%w: prepare private pueued launch: %w", ErrBinding, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start private pueued: %w", ErrBinding, err)
	}
	// The spawning process owns exactly one waiter. Release does not reap a
	// Unix child; an unreaped daemon can keep migration waiting on a zombie.
	// Waiting asynchronously preserves the daemon lifetime beyond this caller.
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("private daemon exited: %v", err)
		}
	}()
	return nil
}

func waitReady(ctx context.Context, client *Client, timeout time.Duration) error {
	if ctx == nil {
		return context.Canceled
	}
	if timeout <= 0 {
		return ErrConfiguration
	}
	var lastErr error
	cleaned := false
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		attemptErr, done := waitReadyAttempt(ctx, client, timeout)
		if attemptErr == nil {
			return nil
		}
		lastErr = attemptErr
		if retry, cleanErr := client.cleanupStatusOverflow(ctx, lastErr, true, cleaned); cleanErr != nil {
			return errors.Join(lastErr, cleanErr)
		} else if retry {
			cleaned = true
			continue
		}
		if done {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitReadyAttempt(ctx context.Context, client *Client, timeout time.Duration) (error, bool) {
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	err := client.Ready(readyContext)
	cancel()
	if err == nil {
		return nil, true
	}
	if pending := pendingFrom(err); pending != nil {
		return processReadyPending(ctx, client, err, pending)
	}
	return err, false
}

type bootstrapLock struct {
	mu          sync.Mutex
	file        *os.File
	transferred bool
	closed      bool
}

func (l *bootstrapLock) duplicateForControlOwner() (*os.File, error) {
	if l == nil {
		return nil, os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.file == nil {
		return nil, os.ErrClosed
	}
	duplicate, err := duplicateCloseOnExec(l.file, "delegate-supervisor-generation-lease")
	if err != nil {
		return nil, err
	}
	// flock is attached to the shared open-file description. Once inherited,
	// the caller must close its descriptor without LOCK_UN; the one-shot owner
	// releases the generation lease after its exact child Wait completes.
	l.transferred = true
	return duplicate, nil
}

func acquireBootstrapLock(ctx context.Context, path string) (*bootstrapLock, error) {
	return acquireSupervisorLock(ctx, path, unix.LOCK_EX)
}

func acquireSupervisorLock(ctx context.Context, path string, mode int) (*bootstrapLock, error) {
	return acquireSupervisorLockMode(ctx, path, mode, true)
}

func acquireExistingSupervisorLock(ctx context.Context, path string, mode int) (*bootstrapLock, error) {
	return acquireSupervisorLockMode(ctx, path, mode, false)
}

func acquireSupervisorLockMode(ctx context.Context, path string, mode int, create bool) (*bootstrapLock, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open supervisor bootstrap lock: %w", ErrBinding, err)
	}
	for {
		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return &bootstrapLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("%w: acquire supervisor bootstrap lock: %w", ErrBinding, err), file.Close())
		}
		if mode&unix.LOCK_NB != 0 {
			if closeErr := file.Close(); closeErr != nil {
				return nil, errors.Join(err, closeErr)
			}
			return nil, errSupervisorLockBusy
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func retainBootstrapLock(lock **bootstrapLock, pending []*Pending, report func(error)) {
	if lock == nil || *lock == nil || len(pending) == 0 {
		return
	}
	retained := *lock
	*lock = nil
	go func() {
		for _, handle := range pending {
			if handle != nil {
				<-handle.Done()
			}
		}
		if err := closeBootstrapLock(retained); err != nil && report != nil {
			report(err)
		}
	}()
}

func finishPrivateBootstrap(lock **bootstrapLock, resultErr *error) {
	retainBootstrapLock(lock, pendingHandles(*resultErr), func(err error) {
		log.Printf("delegate: retained supervisor lock cleanup failed: %v", err)
	})
	*resultErr = errors.Join(*resultErr, closeBootstrapLock(*lock))
}

func closeBootstrapLock(lock *bootstrapLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	var unlockErr error
	if !lock.transferred {
		unlockErr = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	}
	closeErr := lock.file.Close()
	if closeErr == nil {
		lock.closed = true
	}
	return errors.Join(unlockErr, closeErr)
}
