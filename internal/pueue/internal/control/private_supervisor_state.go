package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

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
	alias               privateRecoveryAlias
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
	facts.alias, facts.aliasPresent, facts.aliasErr = readValidatedPrivateRecoveryAlias(stateRoot)
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
	if _, err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return err
	}
	observation, err := c.availableForTransition(ctx, true)
	if err != nil {
		return err
	}
	ready := observation.ready()
	if err := verifyPrivateSupervisorBinding(stateRoot, c.binding, ready); err != nil {
		return errors.Join(err, observation.projectedError())
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
	if err := c.closeRefusedPrivateEndpoint(ctx, observation); err != nil {
		return err
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
	client, found, pairErr, err := preparePublishedPrivate(ctx, stateRoot, options)
	if err != nil {
		return nil, found, err
	}
	if pairErr != nil {
		return nil, true, privatePublishedRecoveryError(pairErr)
	}
	return client, found, nil
}

// preparePublishedPrivate separates the exact published pair's verification
// failure (pairErr) from every other preparation or close fault (err), so a
// recovery owner can test the canonical missing-pair fact without discarding
// an operational fault.
func preparePublishedPrivate(ctx context.Context, stateRoot string, options Options) (client *Client, found bool, pairErr, resultErr error) {
	head, err := readPublishedPrivateHead(stateRoot)
	if err != nil || !head.found {
		return nil, head.found, nil, err
	}
	if !head.complete {
		if head.replayed {
			return nil, true, nil, privateTransitionPostconditionError()
		}
		return nil, true, nil, ErrPrivateSupervisorTransitionIncomplete
	}
	client, pairErr, err = preparePublishedPrivatePair(ctx, head.published, options)
	return client, true, pairErr, err
}

// publishedPrivateHead is the completed-generation fact shared by preparation
// and availability observation: the replayed publication, the published
// binding, and whether every transition of that binding is complete.
type publishedPrivateHead struct {
	published task.SupervisorRef
	found     bool
	replayed  bool
	complete  bool
}

func readPublishedPrivateHead(stateRoot string) (publishedPrivateHead, error) {
	replayed, err := recoverPrivateIdentityPublication(stateRoot)
	if err != nil {
		return publishedPrivateHead{}, privatePublishedRecoveryError(err)
	}
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return publishedPrivateHead{found: found}, err
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, published)
	if err != nil {
		return publishedPrivateHead{}, privatePublishedRecoveryError(err)
	}
	return publishedPrivateHead{published: published, found: true, replayed: replayed, complete: complete}, nil
}

// preparePublishedPrivatePair opens and verifies the exact present published
// pair. A verification failure is pairErr; a close fault after it is err.
func preparePublishedPrivatePair(ctx context.Context, published task.SupervisorRef, options Options) (client *Client, pairErr, resultErr error) {
	client, err := openPublishedPrivatePair(published, options)
	if err != nil {
		return nil, nil, err
	}
	if err = verifyPublishedPrivatePair(ctx, client, published); err != nil {
		if closeErr := client.CloseAfterPending(err); closeErr != nil {
			return nil, nil, errors.Join(privatePublishedRecoveryError(err), closeErr)
		}
		return nil, err, nil
	}
	return client, nil, nil
}

func openPublishedPrivatePair(published task.SupervisorRef, options Options) (*Client, error) {
	options, err := optionsForPublishedPrivateBinding(published, options)
	if err != nil {
		return nil, privatePublishedRecoveryError(err)
	}
	client, err := NewClient(published, options)
	if err != nil {
		return nil, privatePublishedRecoveryError(err)
	}
	return client, nil
}

// PublishedPairAvailability is the clean availability fact of the completed
// published pair. The zero value is no fact and is returned only with an
// error. A fact never authorizes a start, unlink, or publication; it selects
// the existing owner of that state.
type PublishedPairAvailability uint8

const (
	// PublishedPairReady is a complete generation whose present pair verified.
	PublishedPairReady PublishedPairAvailability = iota + 1
	// PublishedPairNotPublished is a root with no published generation.
	PublishedPairNotPublished
	// PublishedPairIncomplete is a published generation with an unfinished
	// transition, restart intent, or launch witness for another pair.
	PublishedPairIncomplete
	// PublishedPairMemberMissing is a complete generation with a recorded
	// executable that no longer exists.
	PublishedPairMemberMissing
	// PublishedPairConfigChanged is a complete, present generation whose
	// canonical config parsed and resolved with the pinned resolver, and whose
	// client and daemon both proved their exact published provenance, but
	// whose config-derived identity changed. Only explicit migration may own
	// it; every other caller refuses with its ProjectedError.
	PublishedPairConfigChanged
)

// ProjectedError is the strict refusal a fact carries for callers that do
// not own it: config drift projects to the exact error that strict published
// verification returns for it. Every other fact projects to nil.
func (availability PublishedPairAvailability) ProjectedError() error {
	if availability == PublishedPairConfigChanged {
		return privatePublishedRecoveryError(ErrBinding)
	}
	return nil
}

// settlePublishedPairAvailability applies the release of the observation's
// client. A close fault discards the fact; its projected refusal stays in the
// error beside the fault.
func settlePublishedPairAvailability(availability PublishedPairAvailability, closeErr error) (PublishedPairAvailability, error) {
	if closeErr == nil {
		return availability, nil
	}
	if projected := availability.ProjectedError(); projected != nil {
		return 0, errors.Join(projected, closeErr)
	}
	return 0, closeErr
}

// ObservePublishedPrivatePairWithLock observes the completed published pair
// while the caller holds WithBootstrapLock. Completeness is decided before
// presence: an incomplete generation, such as a launch witness that names a
// relocated pair, belongs to reconciliation even when the published pair is
// present. Only a complete, present pair is opened and verified, then closed.
// Every read, verification, pending, context, or close fault returns the zero
// fact with its error; config drift is a fact only when nothing faulted.
func ObservePublishedPrivatePairWithLock(ctx context.Context, stateRoot string, options Options) (PublishedPairAvailability, error) {
	head, err := readPublishedPrivateHead(stateRoot)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	if !head.found {
		return PublishedPairNotPublished, nil
	}
	if !head.complete {
		return PublishedPairIncomplete, nil
	}
	missing, err := PrivateSupervisorPairMissing(head.published)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	if missing {
		return PublishedPairMemberMissing, nil
	}
	client, err := openPublishedPrivatePair(head.published, options)
	if err != nil {
		return 0, err
	}
	availability, pairErr := observePublishedPrivatePair(ctx, client, head.published)
	if pairErr != nil {
		if closeErr := client.CloseAfterPending(pairErr); closeErr != nil {
			return 0, errors.Join(privatePublishedRecoveryError(pairErr), closeErr)
		}
		return 0, privatePublishedRecoveryError(pairErr)
	}
	return settlePublishedPairAvailability(availability, client.Close())
}

// observePublishedPrivatePair is the availability form of
// verifyPublishedPrivatePair. Config-only drift does not stop verification:
// the daemon's real bytes are still checked against the published target, and
// the drift is a fact only after both executable proofs and the context
// checks succeed. Any other difference is ErrBinding.
func observePublishedPrivatePair(ctx context.Context, client *Client, published task.SupervisorRef) (PublishedPairAvailability, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	comparison, err := client.compareBinding()
	if err != nil {
		return 0, err
	}
	if comparison == bindingChanged {
		return 0, ErrBinding
	}
	if err = attachPrivateDaemon(client, published.DaemonExecutable); err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if daemon := client.Binding(); daemon.DaemonExecutable != published.DaemonExecutable || daemon.DaemonSHA256 != published.DaemonSHA256 {
		return 0, ErrBinding
	}
	if comparison == bindingConfigChanged {
		return PublishedPairConfigChanged, nil
	}
	return PublishedPairReady, nil
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
	if _, err = recoverPrivateIdentityPublication(stateRoot); err != nil {
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

// bindPrivateTransitionClient returns the endpoint observation beside any
// operational error. On an observation the verified client stays open, so an
// exclusive caller can finish its binding checks before the canonical
// closure. Every operational failure closes the client and returns no client.
func bindPrivateTransitionClient(ctx context.Context, clientPath, daemonPath, configPath string, options Options, cleanupSuccessful bool) (*Client, PrivateEndpointObservation, error) {
	client, err := Bind(ctx, clientPath, configPath, options)
	if err != nil {
		return nil, PrivateEndpointObservation{}, err
	}
	if err = attachPrivateDaemon(client, daemonPath); err != nil {
		return nil, PrivateEndpointObservation{}, errors.Join(err, client.Close())
	}
	_, observation, err := privateClientObserveSnapshot(ctx, client, cleanupSuccessful)
	if err != nil {
		return nil, PrivateEndpointObservation{}, errors.Join(err, client.CloseAfterPending(err))
	}
	return client, observation, nil
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

func (c *Client) endpointProbe(ctx context.Context) (unavailable, refused bool, resultErr error) {
	if c == nil || ctx == nil {
		return false, false, context.Canceled
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return false, false, err
	}
	return privateEndpointProbe(ctx, path)
}

func privateEndpointSocketPath(endpoint string) (string, error) {
	path, invalid := migration.UnixSocketPath(endpoint)
	if invalid == nil {
		return path, nil
	}
	return "", privateDaemonIdentityError(invalid)
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
	if privateDialEndedByCaller(ctx, err) {
		// The caller's own cancellation or deadline ended the dial. That says
		// nothing about the binding, so the dial error keeps its own identity.
		return false, false, err
	}
	return false, false, errors.Join(ErrBinding, err)
}

// privateDialEndedByCaller reports whether a dial error is the caller's own
// termination. The dialer also detects a passed caller deadline on its own,
// from the deadline and the clock, so the dial can report that deadline before
// the caller's context records it; a passed caller deadline therefore also
// matches.
func privateDialEndedByCaller(ctx context.Context, err error) bool {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Is(err, ctxErr)
	}
	deadline, bounded := ctx.Deadline()
	return bounded && !time.Now().Before(deadline) && errors.Is(err, context.DeadlineExceeded)
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
