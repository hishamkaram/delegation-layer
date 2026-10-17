package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
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

// PrivateMigrationTargetsWithLock verifies that an application journal names
// the same successor as the private migration owner. The caller holds the
// bootstrap lock; a staged installation cannot retarget a pending group edge.
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
		privateDaemonIdentity{Executable: target.DaemonExecutable, SHA256: target.DaemonSHA256},
		privateDaemonIdentityForBinding(target)), nil
}

func privateMigrationMayRetarget(record privateMigrationRecord) bool {
	return !record.LegacyShutdownRequested && !record.LegacyShutdownDispatched && !record.LegacyStopped
}

// PrivateMigrationRetargetableWithLock exposes the migration owner's exact
// point of no return. Only the private migration protocol changes the journal.
func PrivateMigrationRetargetableWithLock(root string) (bool, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false, ErrConfiguration
	}
	record, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	return found && privateMigrationMayRetarget(record), err
}

// PrepareRecordedPrivateMigrationWithLock verifies the recorded pair using its
// recorded resolver even if the operator's current HOME/XDG context changed.
func PrepareRecordedPrivateMigrationWithLock(ctx context.Context, root string, target task.SupervisorRef, options Options) (*Client, error) {
	matches, err := PrivateMigrationTargetsWithLock(root, target)
	if err != nil || !matches {
		return nil, privateRecoveryFailure("transition_pending", errors.Join(err, ErrBinding))
	}
	return prepareExactPublishedPair(ctx, target, options)
}

// ReconcilePublishedPrivatePairWithLock repairs only executable locations of
// the published generation. The caller holds the bootstrap lock and proves
// the candidate is the protected, content-addressed pair for its recorded
// hashes, with no pending application-owned inspection transition. It never
// selects a different binary generation or stops a live daemon.
func ReconcilePublishedPrivatePairWithLock(ctx context.Context, stateRoot, exactClient, exactDaemon string, options Options) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recoverPrivateIdentityPublication(stateRoot); err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found {
		return err
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	if !found {
		return nil // Missing provenance belongs to the existing bootstrap owner.
	}
	bootstrap, _, err := readPrivateBootstrapRecord(stateRoot)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	if bootstrap.Restart != nil {
		if bootstrap.Restart.Target.ClientExecutable != exactClient || bootstrap.Restart.Target.DaemonExecutable != exactDaemon {
			return privateRecoveryFailure("identity_conflict", ErrBinding)
		}
		return resumePrivatePairRestart(ctx, stateRoot, published, *bootstrap.Restart, options)
	}
	return reconcilePrivatePairWithWitness(ctx, stateRoot, published, identity, exactClient, exactDaemon, options)
}

func reconcilePrivatePairWithWitness(ctx context.Context, stateRoot string, published task.SupervisorRef, identity privateDaemonIdentity, exactClient, exactDaemon string, options Options) (resultErr error) {
	missing, err := privatePublishedPairMissing(published)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	coherent := privateDaemonIdentityMatchesBinding(identity, published)
	if coherent && !missing {
		return nil
	}
	candidate := published
	candidate.ClientExecutable, candidate.DaemonExecutable = exactClient, exactDaemon
	if err = privatePairReconciliationAllowed(ctx, stateRoot, published, candidate); err != nil {
		return err
	}
	if !coherent && !privateDaemonIdentityMatchesBinding(identity, candidate) {
		return privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	client, err := prepareExactPublishedPair(ctx, candidate, options)
	if err != nil {
		return privateRecoveryFailure("predecessor_pair_unavailable", err)
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	if !coherent {
		// The launch witness already names this exact pair. Only the erroneous
		// binding changes; daemon provenance and task records remain untouched.
		return replaceReconciledPrivateBinding(stateRoot, published, candidate)
	}
	ready, err := client.available(ctx)
	if err != nil {
		return privateRecoveryFailure("observation_failed", err)
	}
	if ready {
		return privateRecoveryFailure("published_pair_unavailable_live", ErrBinding)
	}
	// Record the exact target only after both endpoint and process absence are
	// proven. A crash can then resume this target without relaunching it.
	if err = client.WaitUnavailable(ctx); err != nil {
		return privateRecoveryFailure("observation_failed", err)
	}
	intent := privatePairRestartIntent{Predecessor: published, Target: candidate}
	if err = publishPrivatePairRestartIntent(stateRoot, intent); err != nil {
		return err
	}
	return finishPrivatePairRestart(ctx, stateRoot, client)
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

func publishPrivatePairRestartIntent(root string, intent privatePairRestartIntent) error {
	if err := validatePrivatePairRestartIntent(root, intent); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion, Restart: &intent})
	if err != nil {
		return err
	}
	writeErr := publishPrivateRecord(filepath.Join(root, privateSupervisorDirectory), privateBootstrapRecordPath(root), data)
	winner, found, readErr := readPrivateBootstrapRecord(root)
	if readErr != nil || !found || winner.Restart == nil || *winner.Restart != intent {
		return privateRecoveryFailure("state_unreadable", errors.Join(writeErr, readErr, ErrBinding))
	}
	return syncPrivateDirectory(filepath.Join(root, privateSupervisorDirectory))
}

func resumePrivatePairRestart(ctx context.Context, root string, published task.SupervisorRef, intent privatePairRestartIntent, options Options) (resultErr error) {
	if published != intent.Predecessor && published != intent.Target {
		return privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	// Ignore only this exact bootstrap intent. Other transitions retain their
	// existing owners and cannot be overwritten by restart recovery.
	if pending, err := PrivateSupervisorMigrationPending(root); err != nil || pending {
		return privateRecoveryFailure("transition_pending", errors.Join(err, ErrBinding))
	}
	if alias, err := PrivateRecoveryBindingPresent(root); err != nil || alias {
		return privateRecoveryFailure("transition_pending", errors.Join(err, ErrBinding))
	}
	if err := privatePairHistoryUnreferenced(ctx, root, intent.Predecessor, intent.Target); err != nil {
		return err
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found {
		return privateRecoveryFailure("state_unreadable", errors.Join(err, ErrBinding))
	}
	if !privateDaemonIdentityMatchesBinding(identity, intent.Predecessor) && !privateDaemonIdentityMatchesBinding(identity, intent.Target) {
		return privateRecoveryFailure("identity_conflict", ErrBinding)
	}
	client, err := prepareExactPublishedPair(ctx, intent.Target, options)
	if err != nil {
		return privateRecoveryFailure("predecessor_pair_unavailable", err)
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	return finishPrivatePairRestart(ctx, root, client)
}

func finishPrivatePairRestart(ctx context.Context, root string, client *Client) error {
	ready, err := client.available(ctx)
	if err != nil {
		return privateRecoveryFailure("observation_failed", err)
	}
	if !ready {
		return client.startPrivate(ctx)
	}
	if err = publishPrivateDaemonIdentity(filepath.Join(root, privateSupervisorDirectory), client.Binding()); err != nil {
		return err
	}
	return clearPrivateBootstrapPending(root)
}

func privatePublishedPairMissing(binding task.SupervisorRef) (bool, error) {
	missing := false
	for _, path := range []string{binding.ClientExecutable, binding.DaemonExecutable} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			missing = true
		} else if err != nil {
			return false, err
		}
	}
	return missing, nil
}

func privatePairReconciliationAllowed(ctx context.Context, root string, published, candidate task.SupervisorRef) error {
	pending, err := privateRecoveryTransitionPending(root)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	recovery, err := PrivateRecoveryBindingPresent(root)
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	if pending || recovery {
		return privateRecoveryFailure("transition_pending", ErrBinding)
	}
	return privatePairHistoryUnreferenced(ctx, root, published, candidate)
}

func privatePairHistoryUnreferenced(ctx context.Context, root string, published, candidate task.SupervisorRef) error {
	history, _, err := loadPrivateMigrationHistory(filepath.Join(root, privateSupervisorDirectory))
	if err != nil {
		return privateRecoveryFailure("state_unreadable", err)
	}
	for _, edge := range history.Migrations {
		if err = ctx.Err(); err != nil {
			return err
		}
		for _, binding := range []task.SupervisorRef{published, candidate} {
			if privateDaemonIdentityMatchesBinding(edge.Legacy, binding) || privateDaemonIdentityMatchesBinding(edge.Successor, binding) {
				return privateRecoveryFailure("transition_pending", ErrBinding)
			}
		}
	}
	return nil
}

func prepareExactPublishedPair(ctx context.Context, candidate task.SupervisorRef, options Options) (*Client, error) {
	options, err := optionsForPublishedPrivateBinding(candidate, options)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(candidate, options)
	if err != nil {
		return nil, err
	}
	if err = verifyPublishedPrivatePair(ctx, client, candidate); err != nil {
		return nil, errors.Join(err, client.CloseAfterPending(err))
	}
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
	if _, found, err := loadPrivateSupervisorBinding(root); err != nil {
		return err
	} else if found {
		return publishPrivateSupervisorBindingIfAbsentContext(ctx, root, predecessor)
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
	return publishPrivateSupervisorBindingIfAbsentContext(ctx, root, predecessor)
}
