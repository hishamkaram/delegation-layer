package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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

// PrivateSupervisorMigrationComplete reports whether the published generation
// has completed every transition and recovery phase. Observation of a live
// predecessor does not authorize submissions, repair, or terminal cleanup.
func PrivateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef) (bool, error) {
	return privateSupervisorMigrationComplete(stateRoot, binding, false)
}

// Alias retirement alone may ignore alias state, including partially removed
// artifacts. Its remover still owns record validation and completed history.
func privateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef, allowRecoveryAlias bool) (bool, error) {
	if !IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return false, ErrConfiguration
	}
	facts, err := loadPrivateGenerationFacts(stateRoot)
	if err != nil {
		return false, err
	}
	return facts.complete(binding, allowRecoveryAlias)
}

type privateGenerationPhase uint8

const (
	privateGenerationUnavailable privateGenerationPhase = iota
	privateGenerationObservable
	privateGenerationComplete
)

// The migration owner writes these records under the EX bootstrap lock.
// Readers retain the SH generation lease for the entire decision and probe.
// Alias validity is separate: generic retirement deliberately ignores it only
// after its own validation and history guards, including orphan recovery.
type privateGenerationFacts struct {
	published           task.SupervisorRef
	publishedFound      bool
	publishedErr        error
	identity            privateDaemonIdentity
	identityFound       bool
	identityErr         error
	migration           privateMigrationRecord
	migrationPresent    bool
	migrationErr        error
	alias               privateRecoveryBindingRecord
	aliasPresent        bool
	aliasErr            error
	aliasPublishing     bool
	aliasPublicationErr error
	settled             bool
	transitionErr       error
}

func loadPrivateGenerationFacts(stateRoot string) (privateGenerationFacts, error) {
	var facts privateGenerationFacts
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return facts, ErrConfiguration
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	facts.published, facts.publishedFound, facts.publishedErr = loadPrivateSupervisorBinding(stateRoot)
	facts.migration, facts.migrationPresent, facts.migrationErr = loadPrivateMigrationRecord(base)
	facts.alias, facts.aliasPresent, facts.aliasErr = readPrivateRecoveryBinding(stateRoot)
	facts.aliasPublishing, facts.aliasPublicationErr = privateRecoveryObservationConflict(stateRoot, facts.aliasPresent)
	bootstrapPending, bootstrapErr := PrivateBootstrapPending(stateRoot)
	_, publishing, publicationErr := readPrivateIdentityPublication(stateRoot)
	facts.settled = !bootstrapPending && !publishing
	facts.transitionErr = bootstrapErr
	if bootstrapErr == nil && !bootstrapPending {
		facts.transitionErr = publicationErr
	}
	facts.identity, facts.identityFound, facts.identityErr = loadPrivateDaemonIdentity(base)
	return facts, nil
}

func privateRecoveryObservationConflict(stateRoot string, aliasPresent bool) (bool, error) {
	if !aliasPresent {
		return privateRecoveryAliasArtifactsPresent(stateRoot)
	}
	for _, path := range []string{privateRecoveryPendingRecordPath(stateRoot), privateRecoveryPendingConfigPath(stateRoot)} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("%w: inspect legacy recovery publication: %w", ErrBinding, err)
		}
	}
	return false, nil
}

func (facts privateGenerationFacts) publishedReady(binding task.SupervisorRef) (bool, error) {
	if !facts.settled || facts.transitionErr != nil {
		return false, facts.transitionErr
	}
	if facts.identityErr != nil || !facts.identityFound {
		return false, facts.identityErr
	}
	if !privateDaemonIdentityMatchesBinding(facts.identity, binding) {
		return false, nil
	}
	return facts.publishedFound && task.SameSupervisorIdentity(facts.published, binding), facts.publishedErr
}

func (facts privateGenerationFacts) complete(binding task.SupervisorRef, allowRecoveryAlias bool) (bool, error) {
	if facts.migrationErr != nil || facts.migrationPresent {
		return false, facts.migrationErr
	}
	if !allowRecoveryAlias && (facts.aliasErr != nil || facts.aliasPresent) {
		return false, facts.aliasErr
	}
	return facts.publishedReady(binding)
}

func (facts privateGenerationFacts) phase(binding task.SupervisorRef) (privateGenerationPhase, error) {
	if facts.migrationErr != nil {
		return privateGenerationUnavailable, facts.migrationErr
	}
	ready, err := facts.publishedReady(binding)
	if err != nil || !ready || !privateFullGenerationMatches(facts.published, binding) {
		return privateGenerationUnavailable, err
	}
	if facts.migrationPresent && (!privateMigrationMayRetarget(facts.migration) || !privateDaemonIdentityMatchesBinding(facts.migration.Legacy, binding)) {
		return privateGenerationUnavailable, nil
	}
	aliasMatches, aliasErr := facts.observableAliasMatches(binding)
	if aliasErr != nil || !aliasMatches {
		return privateGenerationUnavailable, aliasErr
	}
	if facts.migrationPresent || facts.aliasPresent {
		return privateGenerationObservable, nil
	}
	return privateGenerationComplete, nil
}

func (facts privateGenerationFacts) observableAliasMatches(binding task.SupervisorRef) (bool, error) {
	if facts.aliasErr != nil || facts.aliasPublicationErr != nil || facts.aliasPublishing {
		return false, errors.Join(facts.aliasErr, facts.aliasPublicationErr)
	}
	return !facts.aliasPresent || (task.SameSupervisorIdentity(facts.alias.Supervisor, binding) && facts.alias.ConfigSHA256 == binding.ConfigDigest), nil
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
	if err = ensurePrivateSupervisorMigrationComplete(stateRoot, published); err != nil {
		return nil, true, err
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
	if published, found, readErr := loadPrivateSupervisorBinding(stateRoot); readErr != nil {
		return nil, readErr
	} else if found {
		options, err = optionsForPublishedPrivateBinding(published, options)
		if err != nil {
			return nil, err
		}
		resolution, err = privateResolution(options)
		if err != nil {
			return nil, err
		}
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
	if cleanErr := c.cleanSuccessfulTasks(ctx, false); cleanErr != nil {
		return false, cleanErr
	}
	return true, nil
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
	path, invalid := migration.UnixSocketPath(endpoint)
	if invalid == nil {
		return path, nil
	}
	return "", privateDaemonIdentityError(invalid)
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
