package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	ErrHistoricalTargetUnsupported = errors.New("unsupported historical private supervisor target")
	ErrHistoricalTargetNotReady    = errors.New("historical private supervisor target is current but not ready")
)

// PrivateRecoveryError retains the cause while giving callers a bounded,
// path-free explanation of a private-supervisor recovery decision.
type PrivateRecoveryError struct {
	ReasonCode string
	Err        error
}

func (e *PrivateRecoveryError) Error() string { return e.Err.Error() }

func (e *PrivateRecoveryError) Unwrap() error { return e.Err }

func privateRecoveryFailure(reason string, err error) error {
	return &PrivateRecoveryError{ReasonCode: reason, Err: errors.Join(ErrPrivateSupervisorMigrationRequired, err)}
}

// PrivatePairRestartPending reports exact restart ownership independently of
// executable availability. Losing a target file never grants a new generation.
func PrivatePairRestartPending(root string) (bool, error) {
	record, found, err := readPrivateBootstrapRecord(root)
	return found && record.Restart != nil, err
}

func refusePendingPrivateRestart(root string) error {
	record, _, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	if runtimeRestartPending(record) {
		return ErrPrivateRuntimeRestartPending
	}
	if record.Restart != nil {
		return privateRecoveryFailure("transition_pending", ErrBinding)
	}
	return nil
}

func validatePrivateRestartPublication(root string, binding task.SupervisorRef) error {
	record, _, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	if runtimeRestartPending(record) {
		return ErrPrivateRuntimeRestartPending
	}
	if record.Restart != nil && record.Restart.Target != binding {
		return privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	return nil
}

// PrivateMigrationTargetsWithLock verifies that an application journal names
// the same successor as the private migration owner. The caller holds the
// bootstrap lock; only the migration owner may replace a reversible target.
func PrivateMigrationTargetsWithLock(root string, target task.SupervisorRef) (bool, error) {
	if !IsPrivateConfig(root, target.ConfigPath) {
		return false, ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(target); err != nil {
		return false, err
	}
	record, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found {
		return false, err
	}
	return privateMigrationIdentityMatchesCurrent(record.Successor,
		privateDaemonIdentityForBinding(target),
		privateDaemonIdentityForBinding(target)), nil
}

func privateMigrationPrepared(record privateMigrationRecord) bool {
	return !record.LegacyShutdownRequested && !record.LegacyShutdownDispatched && !record.LegacyStopped
}

func privateMigrationRetargetable(record privateMigrationRecord) bool {
	return privateMigrationPrepared(record) && record.LegacyPID == 0 && record.LegacyProcessStart == "" &&
		privateLogicalIdentityPresent(record.Legacy) && privateLogicalIdentityPresent(record.Successor)
}

// PrivateMigrationRetargetableWithLock observes whether a journal has never
// authorized shutdown. The caller retains the bootstrap lock; the migration
// owner revalidates the predecessor and replacement before committing.
func PrivateMigrationRetargetableWithLock(root string) (bool, error) {
	if !IsPrivateConfig(root, PrivateConfigPath(root)) {
		return false, ErrConfiguration
	}
	record, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	return found && privateMigrationRetargetable(record), err
}

// PrepareRecordedPrivateMigrationWithLock opens a full target supplied by the
// canonical accepted-record owner. A present private journal must select it
// exactly. Preparation never converts its callback or selects a fallback.
func PrepareRecordedPrivateMigrationWithLock(ctx context.Context, root string, target task.SupervisorRef, options Options) (*Client, error) {
	if !IsPrivateConfig(root, target.ConfigPath) {
		return nil, ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(target); err != nil {
		return nil, err
	}
	pending, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	if err != nil {
		return nil, err
	}
	if found && !privateDaemonIdentityMatchesBinding(pending.Successor, target) {
		return nil, privateRecoveryFailure("transition_pending", ErrBinding)
	}
	return prepareExactPublishedPair(ctx, target, options)
}

// PublishedPairReconciliation is the clean outcome of one published-pair
// reconciliation attempt. The zero value is no fact and is returned only with
// an error. Deferral facts never authorize a start, unlink, or publication;
// the caller's existing policy decides whether to wait for their owner.
type PublishedPairReconciliation uint8

const (
	// PublishedPairReconciled is a completed repair or a pair that needed none.
	PublishedPairReconciled PublishedPairReconciliation = iota + 1
	// PublishedPairTransitionPending is a clean, readable transition that
	// another owner must finish before this repair may run.
	PublishedPairTransitionPending
	// PublishedPairLiveUnavailable is a live daemon whose published pair
	// cannot be restarted in place.
	PublishedPairLiveUnavailable
)

// ProjectedError is the legacy error for a deferral outcome, keeping each
// existing reason code. A reconciled or zero outcome projects to nil.
func (outcome PublishedPairReconciliation) ProjectedError() error {
	switch outcome {
	case PublishedPairReconciled:
		return nil
	case PublishedPairTransitionPending:
		return privateRecoveryFailure("transition_pending", ErrBinding)
	case PublishedPairLiveUnavailable:
		return privateRecoveryFailure("published_pair_unavailable_live", ErrBinding)
	}
	return nil
}

// settlePublishedPairReconciliation applies the release of the attempt's
// client. A close fault discards the outcome; its projected reason stays in
// the error beside the fault.
func settlePublishedPairReconciliation(outcome PublishedPairReconciliation, err, closeErr error) (PublishedPairReconciliation, error) {
	if closeErr == nil {
		return outcome, err
	}
	return 0, errors.Join(err, outcome.ProjectedError(), closeErr)
}

// ReconcilePublishedPrivatePairAttemptWithLock settles recorded restart intent
// before selecting an installed pair for binding-only repair. The caller holds
// the bootstrap lock. This attempt never starts or stops a daemon.
func ReconcilePublishedPrivatePairAttemptWithLock(ctx context.Context, stateRoot string, selectPair CompatiblePairSelector, options Options) (PublishedPairReconciliation, error) {
	if ctx == nil {
		return 0, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if pending, err := PrivateRuntimeRestartPending(ctx, stateRoot); err != nil || pending {
		if err != nil {
			return 0, err
		}
		return 0, ErrPrivateRuntimeRestartPending
	}
	if _, err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	bootstrap, _, err := readPrivateBootstrapRecord(stateRoot)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return 0, err
	}
	if bootstrap.Restart != nil {
		return resumePrivatePairRestart(ctx, stateRoot, published, *bootstrap.Restart, options)
	}
	if !found {
		return PublishedPairReconciled, nil
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	if !found {
		return PublishedPairReconciled, nil // Missing provenance belongs to the existing bootstrap owner.
	}
	return reconcilePrivatePairWithWitness(ctx, stateRoot, published, identity, selectPair, options)
}

func reconcilePrivatePairWithWitness(ctx context.Context, stateRoot string, published task.SupervisorRef, identity privateDaemonIdentity, selectPair CompatiblePairSelector, options Options) (outcome PublishedPairReconciliation, resultErr error) {
	missing, err := PrivateSupervisorPairMissing(published)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	coherent := privateDaemonIdentityMatchesBinding(identity, published)
	if coherent {
		if !missing {
			return PublishedPairReconciled, nil
		}
	} else if published.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
		return 0, privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	if outcome, err = privatePairReconciliationAllowed(ctx, stateRoot, published); err != nil || outcome != PublishedPairReconciled {
		return outcome, err
	}
	candidate, allowed, err := selectPrivateReconciliationCandidate(ctx, stateRoot, published, selectPair)
	if err != nil || allowed != PublishedPairReconciled {
		return allowed, err
	}
	if !coherent && !privateDaemonIdentityMatchesBinding(identity, candidate) {
		return 0, privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	client, err := prepareExactPublishedPair(ctx, candidate, options)
	if err != nil {
		return 0, privateRecoveryFailure("predecessor_pair_unavailable", err)
	}
	defer func() {
		outcome, resultErr = settlePublishedPairReconciliation(outcome, resultErr, client.CloseAfterPending(resultErr))
	}()
	if !coherent {
		// The launch witness already names this exact pair. Only the erroneous
		// binding changes; daemon provenance and task records remain untouched.
		return reconciledOutcome(replaceReconciledPrivateBindingContext(ctx, stateRoot, published, candidate, options.PrivateGenerationGuard))
	}
	observation, err := client.availableForTransition(ctx, false)
	if err != nil {
		return 0, privateRecoveryFailure("observation_failed", err)
	}
	if observation.ready() {
		return PublishedPairLiveUnavailable, nil
	}
	return 0, privateRecoveryFailure("predecessor_pair_unavailable", ErrBinding)
}

func selectPrivateReconciliationCandidate(ctx context.Context, root string, published task.SupervisorRef, selectPair CompatiblePairSelector) (task.SupervisorRef, PublishedPairReconciliation, error) {
	if published.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
		return task.SupervisorRef{}, 0, privateRecoveryFailure("predecessor_pair_unavailable", ErrBinding)
	}
	if selectPair == nil {
		return task.SupervisorRef{}, 0, ErrConfiguration
	}
	client, daemon, err := selectPair(ctx)
	if err != nil {
		return task.SupervisorRef{}, 0, err
	}
	candidate := published
	candidate.ClientExecutable, candidate.DaemonExecutable = client, daemon
	allowed, err := privatePairHistoryUnreferenced(ctx, root, candidate)
	return candidate, allowed, err
}

func reconciledOutcome(err error) (PublishedPairReconciliation, error) {
	if err != nil {
		return 0, err
	}
	return PublishedPairReconciled, nil
}

// The bootstrap journal owns restart intent, not active identity. Only
// readiness-gated publication may promote Target to the active generation.
type privatePairRestartIntent struct {
	Predecessor task.SupervisorRef `json:"predecessor"`
	Target      task.SupervisorRef `json:"target"`
}

func validatePrivatePairRestartIntent(root string, intent privatePairRestartIntent) error {
	for _, binding := range []task.SupervisorRef{intent.Predecessor, intent.Target} {
		if err := validatePrivateSupervisorRef(binding); err != nil {
			return err
		}
		if !IsPrivateConfig(root, binding.ConfigPath) {
			return ErrBinding
		}
	}
	before := intent.Predecessor
	before.ClientExecutable, before.DaemonExecutable = intent.Target.ClientExecutable, intent.Target.DaemonExecutable
	if before != intent.Target || intent.Predecessor == intent.Target {
		return ErrBinding
	}
	return nil
}

func resumePrivatePairRestart(ctx context.Context, root string, published task.SupervisorRef, intent privatePairRestartIntent, options Options) (outcome PublishedPairReconciliation, resultErr error) {
	if published != intent.Predecessor && published != intent.Target {
		return 0, privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	// Ignore only this exact bootstrap intent. Other transitions retain their
	// existing owners and cannot be overwritten by restart recovery. Only a
	// readable record is a deferral; a read fault is never a transition.
	for _, transitionPending := range []func(string) (bool, error){PrivateSupervisorMigrationPending, PrivateRecoveryBindingPresent} {
		pending, err := transitionPending(root)
		if err != nil {
			return 0, privateRecoveryFailure("state_unreadable", err)
		}
		if pending {
			return PublishedPairTransitionPending, nil
		}
	}
	if history, err := privatePairHistoryUnreferenced(ctx, root, intent.Predecessor, intent.Target); err != nil || history != PublishedPairReconciled {
		return history, err
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found {
		return 0, privateRecoveryFailure("state_unreadable", errors.Join(err, ErrBinding))
	}
	if !privateDaemonIdentityMatchesBinding(identity, intent.Predecessor) && !privateDaemonIdentityMatchesBinding(identity, intent.Target) {
		return 0, privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	client, err := prepareExactPublishedPair(ctx, intent.Target, options)
	if err != nil {
		return 0, privateRecoveryFailure("predecessor_pair_unavailable", err)
	}
	defer func() {
		outcome, resultErr = settlePublishedPairReconciliation(outcome, resultErr, client.CloseAfterPending(resultErr))
	}()
	observation, err := client.availableForTransition(ctx, false)
	if err != nil {
		return 0, privateRecoveryFailure("observation_failed", err)
	}
	if !observation.ready() {
		return PublishedPairTransitionPending, nil
	}
	return reconciledOutcome(publishPrivateDaemonIdentityContext(ctx, filepath.Join(root, privateSupervisorDirectory), client.Binding(), client.options.PrivateGenerationGuard))
}

// PrivateSupervisorPairMissing is the canonical missing-pair fact: a recorded
// executable of binding no longer exists. An unrecorded path is not checked,
// and any other stat failure is an error, never absence.
func PrivateSupervisorPairMissing(binding task.SupervisorRef) (bool, error) {
	missing := false
	for _, path := range []string{binding.ClientExecutable, binding.DaemonExecutable} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			missing = true
		} else if err != nil {
			return false, err
		}
	}
	return missing, nil
}

// privatePairReconciliationAllowed returns PublishedPairReconciled when the
// repair may proceed and PublishedPairTransitionPending when a readable
// transition owns the generation.
func privatePairReconciliationAllowed(ctx context.Context, root string, published task.SupervisorRef) (PublishedPairReconciliation, error) {
	pending, err := privateRecoveryTransitionPending(root)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	recovery, err := PrivateRecoveryBindingPresent(root)
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	if pending || recovery {
		return PublishedPairTransitionPending, nil
	}
	return privatePairHistoryUnreferenced(ctx, root, published)
}

func privatePairHistoryUnreferenced(ctx context.Context, root string, bindings ...task.SupervisorRef) (PublishedPairReconciliation, error) {
	history, _, err := loadPrivateMigrationHistory(filepath.Join(root, privateSupervisorDirectory))
	if err != nil {
		return 0, privateRecoveryFailure("state_unreadable", err)
	}
	for _, edge := range history.Migrations {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		for _, binding := range bindings {
			if privateDaemonIdentityMatchesBinding(edge.Legacy, binding) || privateDaemonIdentityMatchesBinding(edge.Successor, binding) {
				return PublishedPairTransitionPending, nil
			}
		}
	}
	return PublishedPairReconciled, nil
}

func prepareExactPublishedPair(ctx context.Context, candidate task.SupervisorRef, options Options) (*Client, error) {
	options, err := optionsForPublishedPrivateBinding(candidate, options)
	if err != nil {
		return nil, err
	}
	if isLegacyStateSupervisorClient(candidate) {
		return prepareCurrentHistoricalPrivateTarget(ctx, candidate, options)
	}
	selected, err := canonicalExecutable(candidate.ClientExecutable)
	if err != nil || selected != candidate.ClientExecutable {
		return nil, errors.Join(ErrBinding, err)
	}
	daemon, err := canonicalExecutable(candidate.DaemonExecutable)
	if err != nil || daemon != candidate.DaemonExecutable {
		return nil, errors.Join(ErrBinding, err)
	}
	options.RuntimeClientExecutable = selected
	client, err := NewClient(candidate, options)
	if err != nil {
		return nil, err
	}
	if err = verifyPublishedPrivatePair(ctx, client, candidate); err != nil {
		return nil, errors.Join(err, client.CloseAfterPending(err))
	}
	qualifyPrivateClientConstruction(client)
	return client, nil
}

func replaceReconciledPrivateBinding(root string, before, candidate task.SupervisorRef) error {
	data, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: candidate})
	if err != nil {
		return err
	}
	writeErr := replacePrivateSupervisorBindingRecord(root, data)
	// A rename can commit before its directory barrier reports failure. Read
	// through the canonical store while still exclusive before advancing.
	winner, found, readErr := loadPrivateSupervisorBinding(root)
	if readErr != nil {
		return privateRecoveryFailure("state_unreadable", errors.Join(writeErr, readErr))
	}
	if found && winner == candidate {
		return syncPrivateDirectory(filepath.Join(root, privateSupervisorDirectory))
	}
	if found && winner == before {
		return privateRecoveryFailure("observation_failed", errors.Join(writeErr, ErrBinding))
	}
	return privateRecoveryFailure("identity_conflict", errors.Join(writeErr, ErrBinding))
}

// PublishVerifiedPrivatePredecessorWithLock adopts an inspection's recorded
// predecessor only when its daemon provenance agrees. It never substitutes a
// prepared successor. Missing provenance retains the original bootstrap path.
func PublishVerifiedPrivatePredecessorWithLock(ctx context.Context, root string, predecessor task.SupervisorRef) error {
	return PublishVerifiedPrivatePredecessorWithOptionsAndLock(ctx, root, predecessor, Options{})
}

// PublishVerifiedPrivatePredecessorWithOptionsAndLock uses resident publication
// authority only if adoption must create a missing logical binding.
func PublishVerifiedPrivatePredecessorWithOptionsAndLock(ctx context.Context, root string, predecessor task.SupervisorRef, options Options) error {
	if _, found, err := loadPrivateSupervisorBinding(root); err != nil {
		return err
	} else if found {
		return publishPrivateSupervisorBindingIfAbsentContext(ctx, root, predecessor, options.PrivateGenerationGuard)
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(root, privateSupervisorDirectory))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if !privateDaemonIdentityMatchesBinding(identity, predecessor) {
		return privateRecoveryFailure("identity_conflict", fmt.Errorf("%w: inspection predecessor does not match daemon provenance", ErrBinding))
	}
	return publishPrivateSupervisorBindingIfAbsentContext(ctx, root, predecessor, options.PrivateGenerationGuard)
}

// PublishVerifiedPrivatePredecessorWithLock uses the authority that constructed
// this client. Existing equivalent predecessors remain observational reads.
func (c *Client) PublishVerifiedPrivatePredecessorWithLock(ctx context.Context, root string, predecessor task.SupervisorRef) error {
	if c == nil {
		return ErrConfiguration
	}
	return PublishVerifiedPrivatePredecessorWithOptionsAndLock(ctx, root, predecessor, c.options)
}

// A historical copied target can only be reused as the exact already-current
// complete generation. Installed transport supplies the actual protocol proof;
// this branch never grants a start or reconstructs historical client fields.
func prepareCurrentHistoricalPrivateTarget(ctx context.Context, target task.SupervisorRef, options Options) (*Client, error) {
	root := filepath.Dir(filepath.Dir(target.ConfigPath))
	published, found, err := loadPrivateSupervisorBinding(root)
	if err != nil {
		return nil, err
	}
	if !found || published != target {
		return nil, ErrHistoricalTargetUnsupported
	}
	complete, err := PrivateSupervisorMigrationComplete(root, target)
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, ErrHistoricalTargetUnsupported
	}
	selected, err := selectRecoveryClient(target, options.RuntimeClientExecutable)
	if err != nil {
		return nil, err
	}
	options.RuntimeClientExecutable = selected
	client, err := NewClient(target, options)
	if err != nil {
		return nil, err
	}
	daemon, err := selectRecoveryDaemon(target, filepath.Join(filepath.Dir(selected), "pueued"))
	if err == nil {
		err = attachPrivateDaemon(client, daemon)
	}
	if err == nil {
		err = client.Ready(ctx)
	}
	if err != nil {
		return nil, errors.Join(ErrHistoricalTargetNotReady, err, client.CloseAfterPending(err))
	}
	return client, nil
}
