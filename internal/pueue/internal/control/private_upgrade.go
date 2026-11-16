package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type (
	UpgradeOperationID = migration.UpgradeOperationID
	UpgradeMode        = migration.UpgradeMode
)

const (
	UpgradeReactive          = migration.UpgradeReactive
	UpgradeOperationRequired = migration.UpgradeOperationRequired
)

type UpgradePhase string

const (
	UpgradeDraining             UpgradePhase = "draining"
	UpgradeAwaitingInstallation UpgradePhase = "awaiting_installation"
	UpgradeAuthorized           UpgradePhase = "authorized"
	UpgradeMigrating            UpgradePhase = "migrating"
)

var (
	ErrPrivateUpgradePending     = errors.New("private supervisor upgrade is pending")
	ErrPrivateUpgradeOperation   = errors.New("private supervisor upgrade operation does not match")
	ErrPrivateUpgradeNotRetained = errors.New("private supervisor upgrade operation is not retained")
)

type PrivateUpgradeError struct {
	ReasonCode string
	Err        error
}

func (e *PrivateUpgradeError) Error() string { return e.ReasonCode + ": " + e.Err.Error() }
func (e *PrivateUpgradeError) Unwrap() error { return e.Err }

// PrivateUpgradeObservation is a journal/history observation. Completed
// identifies the immutable winning operation; it grants no mutation authority.
type PrivateUpgradeObservation struct {
	OperationID UpgradeOperationID `json:"operation_id"`
	Mode        UpgradeMode        `json:"mode"`
	Phase       UpgradePhase       `json:"phase"`
	Predecessor task.SupervisorRef `json:"predecessor"`
	Completed   bool               `json:"completed,omitempty"`
}

type privateUpgradePair struct {
	ClientExecutable string `json:"client_executable"`
	DaemonExecutable string `json:"daemon_executable"`
}

type privateUpgradeOperationRef struct {
	OperationID UpgradeOperationID `json:"operation_id"`
}

// Drain owns OP, authorization and installation selection. Migration owns
// native progress and validates its derived target against SelectedPair.
// All writers hold the actual bootstrap EX and resident commit guard.
type privateUpgradeDrain struct {
	Root                    migration.UpgradeRoot      `json:"root"`
	Predecessor             task.SupervisorRef         `json:"predecessor"`
	OperationID             UpgradeOperationID         `json:"operation_id"`
	Mode                    UpgradeMode                `json:"mode"`
	Phase                   UpgradePhase               `json:"phase"`
	CompletionAuthorization privateUpgradeOperationRef `json:"completion_authorization,omitzero"`
	SelectedPair            privateUpgradePair         `json:"selected_pair,omitzero"`
	MigrationReference      privateUpgradeOperationRef `json:"migration_reference,omitzero"`
}

func ParseUpgradeOperationID(value string) (UpgradeOperationID, error) {
	const prefix = "upgrade-v1:"
	if !strings.HasPrefix(value, prefix) || task.ValidateTaskID(strings.TrimPrefix(value, prefix)) != nil {
		return "", ErrPrivateUpgradeOperation
	}
	return UpgradeOperationID(value), nil
}

func (record privateUpgradeDrain) observation() PrivateUpgradeObservation {
	return PrivateUpgradeObservation{OperationID: record.OperationID, Mode: record.Mode, Phase: record.Phase, Predecessor: record.Predecessor}
}

func upgradeRootSubject(root taskdir.RootPin) migration.UpgradeRoot {
	return migration.UpgradeRoot{Path: root.Root(), ID: root.RootID(), Device: root.Device(), Inode: root.Inode()}
}

func validatePrivateUpgradeDrain(root string, record privateUpgradeDrain) error {
	if record.Root.Path != root {
		return ErrBinding
	}
	if err := migration.ValidateUpgradeSubject(record.Root, record.Predecessor, record.OperationID, record.Mode); err != nil {
		return errors.Join(ErrBinding, err)
	}
	switch record.Phase {
	case UpgradeDraining, UpgradeAwaitingInstallation:
		if record.Phase == UpgradeAwaitingInstallation && record.Mode != UpgradeOperationRequired {
			return ErrBinding
		}
		if record.CompletionAuthorization != (privateUpgradeOperationRef{}) || record.SelectedPair != (privateUpgradePair{}) || record.MigrationReference != (privateUpgradeOperationRef{}) {
			return ErrBinding
		}
		return nil
	case UpgradeAuthorized, UpgradeMigrating:
		return validateAuthorizedPrivateUpgrade(record)
	default:
		return ErrBinding
	}
}

func validateAuthorizedPrivateUpgrade(record privateUpgradeDrain) error {
	ref := privateUpgradeOperationRef{OperationID: record.OperationID}
	if record.Mode == UpgradeOperationRequired && record.CompletionAuthorization != ref || record.Mode == UpgradeReactive && record.CompletionAuthorization != (privateUpgradeOperationRef{}) {
		return ErrBinding
	}
	if record.SelectedPair != (privateUpgradePair{}) {
		for _, path := range []string{record.SelectedPair.ClientExecutable, record.SelectedPair.DaemonExecutable} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
				return ErrBinding
			}
		}
	}
	if record.Phase == UpgradeMigrating {
		if record.MigrationReference != ref || record.SelectedPair == (privateUpgradePair{}) {
			return ErrBinding
		}
	} else if record.MigrationReference != (privateUpgradeOperationRef{}) {
		return ErrBinding
	}
	return nil
}

func checkPrivateUpgradeRoot(ctx context.Context, root taskdir.RootPin) error {
	if ctx == nil {
		return context.Canceled
	}
	actual, err := readControlRootPin(ctx, root.Root())
	if err != nil {
		return err
	}
	if !actual.Equal(root) {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func requirePrivateUpgradeAuthority(ctx context.Context, root taskdir.RootPin, options Options) error {
	if options.PrivateGenerationGuard == nil {
		return ErrInfrastructureOwnerRequired
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root.Root()); err != nil {
		return err
	}
	return checkPrivateUpgradeRoot(ctx, root)
}

func readPrivateUpgrade(root taskdir.RootPin, op UpgradeOperationID) (privateUpgradeDrain, bool, error) {
	bootstrap, found, err := readPrivateBootstrapRecord(root.Root())
	if err != nil || !found || bootstrap.DrainV1 == nil {
		return privateUpgradeDrain{}, false, err
	}
	record := *bootstrap.DrainV1
	if record.Root != upgradeRootSubject(root) {
		return privateUpgradeDrain{}, false, task.ErrIdentityMismatch
	}
	if op != "" && record.OperationID != op {
		return privateUpgradeDrain{}, false, ErrPrivateUpgradeOperation
	}
	return record, true, nil
}

func ObservePrivateUpgrade(ctx context.Context, root taskdir.RootPin, op UpgradeOperationID) (PrivateUpgradeObservation, bool, error) {
	if op != "" {
		if _, err := ParseUpgradeOperationID(string(op)); err != nil {
			return PrivateUpgradeObservation{}, false, err
		}
	}
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return PrivateUpgradeObservation{}, false, err
	}
	observation, found, err := observePrivateUpgradeRecords(ctx, root, op)
	rootErr := checkPrivateUpgradeRoot(ctx, root)
	if err != nil || rootErr != nil {
		return PrivateUpgradeObservation{}, false, errors.Join(err, rootErr)
	}
	return observation, found, nil
}

func observePrivateUpgradeRecords(ctx context.Context, root taskdir.RootPin, op UpgradeOperationID) (PrivateUpgradeObservation, bool, error) {
	record, found, err := readPrivateUpgrade(root, op)
	if err == nil && found {
		return record.observation(), true, ctx.Err()
	}
	// A current operation does not erase the immutable completed OP history.
	if err != nil && !errors.Is(err, ErrPrivateUpgradeOperation) {
		return PrivateUpgradeObservation{}, false, err
	}
	if op == "" {
		return PrivateUpgradeObservation{}, false, err
	}
	history, _, readErr := loadPrivateMigrationHistory(filepath.Join(root.Root(), privateSupervisorDirectory))
	if readErr != nil {
		return PrivateUpgradeObservation{}, false, readErr
	}
	for _, completed := range history.Migrations {
		u := completed.UpgradeGeneration
		if u.OperationID == op && u.Root == upgradeRootSubject(root) && u.Completed {
			return PrivateUpgradeObservation{OperationID: op, Mode: u.Mode, Phase: UpgradeMigrating, Predecessor: u.Predecessor, Completed: true}, true, ctx.Err()
		}
	}
	if err != nil {
		return PrivateUpgradeObservation{}, false, err
	}
	return PrivateUpgradeObservation{}, false, ctx.Err()
}

func BeginPrivateUpgradeWithLock(ctx context.Context, root taskdir.RootPin, mode UpgradeMode, options Options) (PrivateUpgradeObservation, error) {
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return PrivateUpgradeObservation{}, err
	}
	if mode != UpgradeReactive && mode != UpgradeOperationRequired {
		return PrivateUpgradeObservation{}, ErrBinding
	}
	if record, found, err := readPrivateUpgrade(root, ""); err != nil {
		return PrivateUpgradeObservation{}, err
	} else if found {
		err = guardPrivateGeneration(ctx, root.Root(), options.PrivateGenerationGuard, func() error { return checkPrivateUpgradeRoot(ctx, root) })
		return record.observation(), err
	}
	facts, err := loadPrivateGenerationFacts(root.Root())
	if err != nil {
		return PrivateUpgradeObservation{}, err
	}
	if !facts.publishedFound {
		return PrivateUpgradeObservation{}, ErrPrivateSupervisorNotInitialized
	}
	complete, err := facts.complete(facts.published, false)
	if err != nil {
		return PrivateUpgradeObservation{}, err
	}
	if !complete {
		return PrivateUpgradeObservation{}, ErrPrivateSupervisorTransitionIncomplete
	}
	id, err := randomid.NewRandomID()
	if err != nil {
		return PrivateUpgradeObservation{}, err
	}
	candidate := privateUpgradeDrain{Root: upgradeRootSubject(root), Predecessor: facts.published, OperationID: UpgradeOperationID("upgrade-v1:" + id), Mode: mode, Phase: UpgradeDraining}
	accepted, err := commitPrivateUpgradeBegin(ctx, root, candidate, options)
	if accepted.OperationID == "" {
		return PrivateUpgradeObservation{}, err
	}
	return accepted.observation(), err
}

func commitPrivateUpgradeBegin(ctx context.Context, root taskdir.RootPin, candidate privateUpgradeDrain, options Options) (privateUpgradeDrain, error) {
	var accepted privateUpgradeDrain
	err := guardPrivateGeneration(ctx, root.Root(), options.PrivateGenerationGuard, func() error {
		var acceptErr error
		accepted, acceptErr = acceptPrivateUpgradeWithinCommit(ctx, root, candidate, options)
		return acceptErr
	})
	return accepted, err
}

func acceptPrivateUpgradeWithinCommit(ctx context.Context, root taskdir.RootPin, candidate privateUpgradeDrain, options Options) (privateUpgradeDrain, error) {
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return privateUpgradeDrain{}, err
	}
	current, found, err := readPrivateBootstrapRecord(root.Root())
	if err != nil {
		return privateUpgradeDrain{}, err
	}
	if found && (current.DrainV1 != nil || current.Restart != nil || runtimeRestartPending(current)) {
		return privateUpgradeDrain{}, ErrPrivateSupervisorTransitionIncomplete
	}
	fresh, err := loadPrivateGenerationFacts(root.Root())
	if err != nil {
		return privateUpgradeDrain{}, err
	}
	complete, err := fresh.complete(candidate.Predecessor, false)
	if err != nil || !complete || fresh.published != candidate.Predecessor {
		return privateUpgradeDrain{}, errors.Join(ErrPrivateSupervisorTransitionIncomplete, err)
	}
	writeErr := writePrivateUpgrade(root.Root(), candidate, found)
	winner, committed, readErr := readPrivateUpgrade(root, candidate.OperationID)
	if readErr != nil || !committed || winner != candidate {
		return privateUpgradeDrain{}, errors.Join(writeErr, readErr, ErrBinding)
	}
	return winner, writeErr
}

func writePrivateUpgrade(root string, record privateUpgradeDrain, replace bool) error {
	if err := validatePrivateUpgradeDrain(root, record); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion, DrainV1: &record})
	if err != nil {
		return err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	if !replace {
		return publishPrivateRecord(base, privateBootstrapRecordPath(root), data)
	}
	return replacePrivateRecordAtomic(base, privateBootstrapRecordPath(root), ".private-upgrade-", "private upgrade journal", data, true)
}

func replacePrivateUpgrade(ctx context.Context, root taskdir.RootPin, expected, replacement privateUpgradeDrain, options Options) error {
	if err := validatePrivateUpgradeTransition(expected, replacement); err != nil {
		return err
	}
	return guardPrivateGeneration(ctx, root.Root(), options.PrivateGenerationGuard, func() error {
		if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
			return err
		}
		current, found, err := readPrivateUpgrade(root, expected.OperationID)
		if err != nil || !found || current != expected {
			return errors.Join(ErrBinding, err)
		}
		return writePrivateUpgradeReplacement(root, replacement)
	})
}

func writePrivateUpgradeReplacement(root taskdir.RootPin, replacement privateUpgradeDrain) error {
	writeErr := writePrivateUpgrade(root.Root(), replacement, true)
	winner, found, readErr := readPrivateUpgrade(root, replacement.OperationID)
	if readErr != nil || !found || winner != replacement {
		return errors.Join(writeErr, readErr, ErrBinding)
	}
	return writeErr
}

// Only this writer may replace a missing candidate before the migration owner
// has accepted a target. Ordinary upgrade progress remains strictly immutable.
func reselectPrivateUpgradePair(ctx context.Context, root taskdir.RootPin, expected, replacement privateUpgradeDrain, options Options) error {
	if err := validatePrivateUpgradeReselection(expected, replacement); err != nil {
		return err
	}
	return guardPrivateGeneration(ctx, root.Root(), options.PrivateGenerationGuard, func() error {
		if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
			return err
		}
		current, found, err := readPrivateUpgrade(root, expected.OperationID)
		if err != nil || !found || current != expected {
			return errors.Join(ErrBinding, err)
		}
		eligible, err := privateUpgradeReselectionEligible(root, expected, options)
		if err != nil || !eligible {
			return errors.Join(ErrBinding, err)
		}
		if err = validatePrivateUpgradePair(replacement.SelectedPair); err != nil {
			return err
		}
		return writePrivateUpgradeReplacement(root, replacement)
	})
}

func validatePrivateUpgradeReselection(before, after privateUpgradeDrain) error {
	if before.Phase != UpgradeAuthorized || after.Phase != UpgradeAuthorized || before.MigrationReference != (privateUpgradeOperationRef{}) ||
		before.SelectedPair == (privateUpgradePair{}) || after.SelectedPair == (privateUpgradePair{}) || before.SelectedPair == after.SelectedPair {
		return ErrBinding
	}
	unchanged := before
	unchanged.SelectedPair = after.SelectedPair
	if unchanged != after {
		return ErrBinding
	}
	return nil
}

// This decision performs canonical reads only. Probes and root-wide queue
// authorization remain outside the resident's non-reentrant commit guard.
func privateUpgradeReselectionEligible(root taskdir.RootPin, drain privateUpgradeDrain, options Options) (bool, error) {
	missing, err := privateUpgradePairMissing(drain.SelectedPair)
	if err != nil || !missing {
		return false, err
	}
	if drain.Phase != UpgradeAuthorized || drain.MigrationReference != (privateUpgradeOperationRef{}) {
		return false, ErrBinding
	}
	facts, err := loadPrivateUpgradeReselectionFacts(root.Root(), drain)
	if err != nil {
		return false, err
	}
	history, _, err := loadPrivateMigrationHistory(filepath.Join(root.Root(), privateSupervisorDirectory))
	if err != nil {
		return false, err
	}
	for _, record := range history.Migrations {
		if record.UpgradeGeneration.OperationID == drain.OperationID {
			return false, ErrBinding
		}
	}
	if err = validatePrivateUpgradeReselectionConfig(root.Root(), drain.Predecessor, facts.aliasPresent, options); err != nil {
		return false, err
	}
	return true, nil
}

func loadPrivateUpgradeReselectionFacts(root string, drain privateUpgradeDrain) (privateGenerationFacts, error) {
	facts, err := loadPrivateGenerationFacts(root)
	if err != nil {
		return facts, err
	}
	if facts.migrationPresent || facts.migrationErr != nil || facts.drain == nil || *facts.drain != drain || facts.published != drain.Predecessor {
		return facts, errors.Join(ErrBinding, facts.migrationErr, facts.publishedErr)
	}
	phase, err := facts.phaseFor(drain.Predecessor, privateGenerationUpgradeControl)
	if err != nil || phase != privateGenerationObservable {
		return facts, errors.Join(ErrBinding, err)
	}
	if _, publishing, publicationErr := readPrivateIdentityPublication(root); publicationErr != nil || publishing {
		return facts, errors.Join(ErrBinding, publicationErr)
	}
	return facts, nil
}

func validatePrivateUpgradeReselectionConfig(root string, predecessor task.SupervisorRef, aliasPresent bool, options Options) error {
	options, err := optionsForSavedBinding(predecessor, options)
	if err != nil {
		return err
	}
	if err = validateSavedPrivateProbeConfig(root, predecessor, options); err != nil || aliasPresent {
		// The generation owner already authenticated an alias. Primary config
		// may contain the callback replacement that alias preparation owns.
		return err
	}
	client, err := NewClient(predecessor, options)
	if err != nil {
		return err
	}
	authenticated, configErr := client.runtimeConfigAuthenticated()
	closeErr := client.Close()
	if !authenticated {
		return errors.Join(ErrBinding, configErr, closeErr)
	}
	return errors.Join(configErr, closeErr)
}

func privateUpgradePairMissing(pair privateUpgradePair) (bool, error) {
	missing := false
	for _, path := range []string{pair.ClientExecutable, pair.DaemonExecutable} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			missing = true
			continue
		} else if err != nil {
			return false, err
		}
		canonical, err := canonicalExecutable(path)
		if err != nil || canonical != path {
			return false, errors.Join(ErrBinding, err)
		}
	}
	return missing, nil
}

func validatePrivateUpgradePair(pair privateUpgradePair) error {
	for _, path := range []string{pair.ClientExecutable, pair.DaemonExecutable} {
		canonical, err := canonicalExecutable(path)
		if err != nil || canonical != path {
			return errors.Join(ErrBinding, err)
		}
	}
	return nil
}

func validatePrivateUpgradeTransition(before, after privateUpgradeDrain) error {
	if before.Root != after.Root || before.OperationID != after.OperationID || before.Mode != after.Mode || before.Predecessor != after.Predecessor {
		return ErrBinding
	}
	if before.SelectedPair != (privateUpgradePair{}) && before.SelectedPair != after.SelectedPair {
		return ErrBinding
	}
	if before.CompletionAuthorization != (privateUpgradeOperationRef{}) && before.CompletionAuthorization != after.CompletionAuthorization {
		return ErrBinding
	}
	if before.MigrationReference != (privateUpgradeOperationRef{}) && before.MigrationReference != after.MigrationReference {
		return ErrBinding
	}
	if !privateUpgradePhaseAdvances(before.Phase, after.Phase) {
		return ErrBinding
	}
	return nil
}

func privateUpgradePhaseAdvances(before, after UpgradePhase) bool {
	if before == after {
		return true
	}
	switch before {
	case UpgradeDraining:
		return after == UpgradeAuthorized || after == UpgradeAwaitingInstallation
	case UpgradeAwaitingInstallation:
		return after == UpgradeAuthorized
	case UpgradeAuthorized:
		return after == UpgradeMigrating
	case UpgradeMigrating:
		return false
	default:
		return false
	}
}

func validatePrivateUpgradeProgress(before, after privateMigrationRecord) error {
	u, v := before.UpgradeGeneration, after.UpgradeGeneration
	if u.Target != v.Target || u.OperationID != v.OperationID || u.LegacyBootID != v.LegacyBootID {
		return ErrBinding
	}
	if before.LegacyPID != 0 && (before.LegacyPID != after.LegacyPID || before.LegacyProcessStart != after.LegacyProcessStart) {
		return ErrBinding
	}
	if before.LegacyStopped && !after.LegacyStopped {
		return ErrBinding
	}
	return validatePrivateUpgradeSuccessorProgress(u, v)
}

func validatePrivateUpgradeSuccessorProgress(before, after migration.UpgradeGeneration) error {
	if before.AcceptedStart && !after.AcceptedStart || before.Completed && !after.Completed {
		return ErrBinding
	}
	if before.SuccessorPID != 0 && (before.SuccessorPID != after.SuccessorPID || before.SuccessorProcessStart != after.SuccessorProcessStart || before.SuccessorBootID != after.SuccessorBootID) {
		return ErrBinding
	}
	return nil
}

// Atomic replacement may commit before its directory barrier reports a fault.
// The same store, EX and guard identify the canonical winner before returning.
func settlePrivateUpgradeMigrationWrite(base string, before, after privateMigrationRecord, writeErr error) error {
	winner, found, readErr := loadPrivateMigrationRecord(base)
	if readErr != nil {
		return errors.Join(writeErr, readErr)
	}
	if found && winner == after {
		return writeErr
	}
	if writeErr != nil && (found && winner == before || !found && before == (privateMigrationRecord{})) {
		return writeErr
	}
	return errors.Join(writeErr, ErrBinding)
}

func AdvancePrivateUpgradeDrainWithLock(ctx context.Context, root taskdir.RootPin, op UpgradeOperationID, options Options) (PrivateUpgradeObservation, error) {
	if op == "" {
		return PrivateUpgradeObservation{}, ErrPrivateUpgradeOperation
	}
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return PrivateUpgradeObservation{}, err
	}
	record, found, err := readPrivateUpgrade(root, op)
	if err != nil || !found {
		return PrivateUpgradeObservation{}, errors.Join(ErrPrivateUpgradeNotRetained, err)
	}
	if record.Phase != UpgradeDraining {
		return record.observation(), nil
	}
	client, err := preparePrivateUpgradePredecessor(ctx, root, record, options)
	if err != nil {
		return record.observation(), err
	}
	authErr := client.authorizePrivateMigrationQueue(ctx, root.Root(), client)
	if closeErr := client.CloseAfterPending(authErr); authErr != nil || closeErr != nil {
		return record.observation(), errors.Join(authErr, closeErr)
	}
	updated := record
	updated.Phase = UpgradeAuthorized
	if record.Mode == UpgradeOperationRequired {
		updated.Phase = UpgradeAwaitingInstallation
	}
	if err = replacePrivateUpgrade(ctx, root, record, updated, options); err != nil {
		return record.observation(), err
	}
	return updated.observation(), nil
}

func AuthorizePrivateUpgradeCompletionWithLock(ctx context.Context, root taskdir.RootPin, op UpgradeOperationID, options Options) (PrivateUpgradeObservation, error) {
	if op == "" {
		return PrivateUpgradeObservation{}, ErrPrivateUpgradeOperation
	}
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return PrivateUpgradeObservation{}, err
	}
	if observed, found, err := ObservePrivateUpgrade(ctx, root, op); err != nil {
		return PrivateUpgradeObservation{}, err
	} else if found && observed.Completed {
		return observed, nil
	}
	record, found, err := readPrivateUpgrade(root, op)
	if err != nil || !found {
		return PrivateUpgradeObservation{}, errors.Join(ErrPrivateUpgradeNotRetained, err)
	}
	if record.Phase == UpgradeAuthorized || record.Phase == UpgradeMigrating {
		return record.observation(), nil
	}
	if record.Phase != UpgradeAwaitingInstallation {
		return record.observation(), upgradePending("drain_incomplete", nil)
	}
	updated := record
	updated.Phase = UpgradeAuthorized
	updated.CompletionAuthorization = privateUpgradeOperationRef{OperationID: op}
	if err = replacePrivateUpgrade(ctx, root, record, updated, options); err != nil {
		return record.observation(), err
	}
	return updated.observation(), nil
}

func upgradePending(reason string, cause error) error {
	return &PrivateUpgradeError{ReasonCode: reason, Err: errors.Join(ErrPrivateUpgradePending, cause)}
}

func preparePrivateUpgradePredecessor(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, options Options) (*Client, error) {
	client, err := bindRecordedPredecessorTransport(ctx, root.Root(), drain.Predecessor, options)
	if err != nil {
		return nil, err
	}
	observation, probeErr := client.ObservePrivateEndpoint(ctx)
	if probeErr != nil || !observation.ready() {
		return nil, errors.Join(upgradePending("predecessor_unavailable", probeErr), client.CloseAfterPending(probeErr))
	}
	return client, nil
}

// CompletePrivateUpgradeWithLock consumes existing authorization only. Plain
// ensure may resume it after a lost acknowledgement; it cannot authorize it.
func CompletePrivateUpgradeWithLock(ctx context.Context, root taskdir.RootPin, op UpgradeOperationID, selectPair CompatiblePairSelector, options Options) (_ *Client, observation PrivateUpgradeObservation, resultErr error) {
	if op == "" {
		return nil, observation, ErrPrivateUpgradeOperation
	}
	if err := requirePrivateUpgradeAuthority(ctx, root, options); err != nil {
		return nil, observation, err
	}
	observed, found, err := ObservePrivateUpgrade(ctx, root, op)
	if err != nil || !found {
		return nil, observation, errors.Join(ErrPrivateUpgradeNotRetained, err)
	}
	if observed.Completed {
		return nil, observed, nil
	}
	drain, found, err := readPrivateUpgrade(root, op)
	if err != nil || !found {
		return nil, observation, errors.Join(ErrPrivateUpgradeNotRetained, err)
	}
	observation = drain.observation()
	if drain.Phase != UpgradeAuthorized && drain.Phase != UpgradeMigrating {
		return nil, observation, upgradePending("completion_not_authorized", nil)
	}
	if _, ok := options.DaemonWaitOwner.(DaemonWaitSettlementOwner); !ok {
		return nil, observation, ErrInfrastructureOwnerRequired
	}
	return resumeAuthorizedPrivateUpgrade(ctx, root, drain, selectPair, options)
}

func resumeAuthorizedPrivateUpgrade(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, selectPair CompatiblePairSelector, options Options) (*Client, PrivateUpgradeObservation, error) {
	observation := drain.observation()
	pending, present, err := loadPrivateMigrationRecord(filepath.Join(root.Root(), privateSupervisorDirectory))
	if err != nil {
		return nil, observation, err
	}
	if !present && drain.Phase == UpgradeMigrating {
		completed, finishErr := finishRecordedPrivateUpgrade(ctx, root, drain, options)
		return nil, completed, finishErr
	}
	if !present {
		drain, pending, err = preparePrivateUpgradeMigration(ctx, root, drain, selectPair, options)
		if err != nil {
			return nil, observation, err
		}
	}
	drain, err = linkPrivateUpgradeMigration(ctx, root, drain, pending, options)
	if err != nil {
		return nil, observation, err
	}
	if pending.UpgradeGeneration.Completed {
		if err = commitPrivateUpgradeCompletion(ctx, root.Root(), pending, options); err != nil {
			return nil, drain.observation(), err
		}
		completed, found, readErr := ObservePrivateUpgrade(ctx, root, drain.OperationID)
		if readErr != nil || !found {
			return nil, drain.observation(), errors.Join(ErrBinding, readErr)
		}
		cleanupErr := retirePrivateRecoveryBinding(ctx, root.Root(), pending.UpgradeGeneration.Target, nil, options.PrivateGenerationGuard)
		return nil, completed, cleanupErr
	}
	return completeLinkedPrivateUpgrade(ctx, root, drain, pending, options)
}

func preparePrivateUpgradeMigration(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, selector CompatiblePairSelector, options Options) (_ privateUpgradeDrain, _ privateMigrationRecord, resultErr error) {
	drain, err := selectPrivateUpgradePair(ctx, root, drain, selector, options)
	if err != nil {
		return drain, privateMigrationRecord{}, err
	}
	client, err := preparePrivateUpgradeTarget(ctx, root, drain, options)
	if err != nil {
		return drain, privateMigrationRecord{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	boot, err := lifetime.BootID()
	if err != nil {
		return drain, privateMigrationRecord{}, err
	}
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(drain.Predecessor), Successor: privateDaemonIdentityForBinding(client.Binding()), UpgradeGeneration: migration.UpgradeGeneration{OperationID: drain.OperationID, Root: drain.Root, Predecessor: drain.Predecessor, Mode: drain.Mode, Target: client.Binding(), LegacyBootID: boot}}
	err = publishPrivateMigrationStateContext(ctx, filepath.Join(root.Root(), privateSupervisorDirectory), pending, options.PrivateGenerationGuard)
	return drain, pending, err
}

func linkPrivateUpgradeMigration(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, pending privateMigrationRecord, options Options) (privateUpgradeDrain, error) {
	if err := validatePrivateUpgradeMigration(ctx, root, pending); err != nil {
		return drain, err
	}
	if drain.Phase == UpgradeMigrating {
		return drain, nil
	}
	updated := drain
	updated.Phase = UpgradeMigrating
	updated.MigrationReference = privateUpgradeOperationRef{OperationID: drain.OperationID}
	if err := replacePrivateUpgrade(ctx, root, drain, updated, options); err != nil {
		return drain, err
	}
	return updated, nil
}

func completeLinkedPrivateUpgrade(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, pending privateMigrationRecord, options Options) (_ *Client, _ PrivateUpgradeObservation, resultErr error) {
	options, err := optionsForSavedBinding(pending.UpgradeGeneration.Target, options)
	if err != nil {
		return nil, drain.observation(), err
	}
	client, err := prepareExactPublishedPair(ctx, pending.UpgradeGeneration.Target, options)
	if err != nil {
		return nil, drain.observation(), err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr))
		}
	}()
	if err = client.MigratePrivateSupervisorWithLock(ctx, root.Root()); err != nil {
		return nil, drain.observation(), fmt.Errorf("advance authorized upgrade generation: %w", err)
	}
	completed, found, err := ObservePrivateUpgrade(ctx, root, drain.OperationID)
	if err != nil || !found || !completed.Completed {
		return nil, drain.observation(), errors.Join(ErrBinding, err)
	}
	return client, completed, nil
}

// A crash after migration-history commit and compare-and-remove can leave only
// Drain. The exact immutable completion and published target own its cleanup;
// no selected executable is opened and no native work is repeated.
func finishRecordedPrivateUpgrade(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, options Options) (PrivateUpgradeObservation, error) {
	base := filepath.Join(root.Root(), privateSupervisorDirectory)
	history, _, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return drain.observation(), err
	}
	for _, record := range history.Migrations {
		if record.UpgradeGeneration.OperationID != drain.OperationID {
			continue
		}
		if err = validatePrivateUpgradeMigration(ctx, root, record); err != nil {
			return drain.observation(), err
		}
		err = guardPrivateGeneration(ctx, root.Root(), options.PrivateGenerationGuard, func() error {
			complete, readErr := privateMigrationCompletionRecorded(root.Root(), base, record.UpgradeGeneration.Target, record)
			if readErr != nil || !complete {
				return errors.Join(ErrBinding, readErr)
			}
			return clearPrivateUpgradeWithinCommit(ctx, root, drain)
		})
		if err != nil {
			return drain.observation(), err
		}
		completed := drain.observation()
		completed.Completed = true
		return completed, nil
	}
	return drain.observation(), upgradePending("migration_evidence_missing", ErrBinding)
}

func selectPrivateUpgradePair(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, selector CompatiblePairSelector, options Options) (privateUpgradeDrain, error) {
	reselect := drain.SelectedPair != (privateUpgradePair{})
	if reselect {
		eligible, err := privateUpgradeReselectionEligible(root, drain, options)
		if err != nil || !eligible {
			return drain, err
		}
	}
	// Full application quiescence precedes target selection and callback I/O.
	previous, err := preparePrivateUpgradePredecessor(ctx, root, drain, options)
	if err != nil {
		return drain, err
	}
	authErr := previous.authorizePrivateMigrationQueue(ctx, root.Root(), previous)
	if closeErr := previous.CloseAfterPending(authErr); authErr != nil || closeErr != nil {
		return drain, errors.Join(authErr, closeErr)
	}
	if selector == nil {
		return drain, ErrConfiguration
	}
	client, daemon, err := selector(ctx)
	if err != nil {
		return drain, err
	}
	updated := drain
	updated.SelectedPair = privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	if err = validatePrivateUpgradePair(updated.SelectedPair); err != nil {
		return drain, err
	}
	if reselect {
		err = reselectPrivateUpgradePair(ctx, root, drain, updated, options)
	} else {
		err = replacePrivateUpgrade(ctx, root, drain, updated, options)
	}
	if err != nil {
		return drain, err
	}
	return updated, nil
}

type (
	privateUpgradeCallbackKey struct{}
	privateUpgradeCallback    struct {
		root  taskdir.RootPin
		drain privateUpgradeDrain
	}
)

func preparePrivateUpgradeTarget(ctx context.Context, root taskdir.RootPin, drain privateUpgradeDrain, options Options) (_ *Client, resultErr error) {
	options, err := optionsForSavedBinding(drain.Predecessor, options)
	if err != nil {
		return nil, err
	}
	client, err := Bind(ctx, drain.SelectedPair.ClientExecutable, drain.Predecessor.ConfigPath, options)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	if err = attachPrivateDaemon(client, drain.SelectedPair.DaemonExecutable); err != nil {
		return nil, err
	}
	callbackContext := context.WithValue(ctx, privateUpgradeCallbackKey{}, privateUpgradeCallback{root: root, drain: drain})
	prepared, err := preparePrivateInstalledCallback(callbackContext, root.Root(), client)
	if err != nil {
		return nil, err
	}
	if prepared != client {
		return prepared, nil
	}
	// The caller receives an independently owned client, not a closed handle.
	return prepareExactPublishedPair(ctx, client.Binding(), options)
}

func (c *Client) recoverPrivateCallbackPredecessor(ctx context.Context, root string, published task.SupervisorRef, previous *Client) (*Client, bool, error) {
	if scoped, err := privateUpgradeCallbackPredecessor(ctx, root, previous); scoped {
		return previous, false, err
	}
	return c.recoverStoppedPrivateCallback(ctx, root, published, previous)
}

func privateUpgradeCallbackPredecessor(ctx context.Context, root string, previous *Client) (bool, error) {
	scope, ok := ctx.Value(privateUpgradeCallbackKey{}).(privateUpgradeCallback)
	if !ok {
		return false, nil
	}
	if scope.root.Root() != root || scope.drain.Phase != UpgradeAuthorized || scope.drain.SelectedPair == (privateUpgradePair{}) {
		return true, ErrBinding
	}
	if err := requirePrivateUpgradeAuthority(ctx, scope.root, previous.options); err != nil {
		return true, err
	}
	current, found, err := readPrivateUpgrade(scope.root, scope.drain.OperationID)
	if err != nil || !found || current != scope.drain {
		return true, errors.Join(ErrBinding, err)
	}
	observation, err := previous.ObservePrivateEndpoint(ctx)
	if err != nil || !observation.ready() {
		return true, errors.Join(upgradePending("predecessor_unavailable", nil), err)
	}
	return true, nil
}

func validatePrivateUpgradeMigration(ctx context.Context, root taskdir.RootPin, record privateMigrationRecord) error {
	u := record.UpgradeGeneration
	if u.OperationID == "" {
		return ErrBinding
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root.Root()); err != nil {
		return err
	}
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return err
	}
	drain, found, err := readPrivateUpgrade(root, u.OperationID)
	if err != nil || !found {
		return errors.Join(ErrBinding, err)
	}
	if drain.Phase != UpgradeAuthorized && drain.Phase != UpgradeMigrating || drain.Root != u.Root || drain.Predecessor != u.Predecessor || drain.Mode != u.Mode ||
		drain.SelectedPair.ClientExecutable != u.Target.ClientExecutable || drain.SelectedPair.DaemonExecutable != u.Target.DaemonExecutable {
		return ErrBinding
	}
	return nil
}

func (c *Client) requirePrivateUpgradePredecessorClosed(ctx context.Context, base string, record privateMigrationRecord) error {
	if record.LegacyStopped {
		return nil
	}
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitSettlementOwner)
	if !ok {
		return ErrInfrastructureOwnerRequired
	}
	process, err := lifetime.NewProcessRef(record.LegacyPID, record.LegacyProcessStart)
	if err != nil {
		return err
	}
	owned, closed, err := owner.ObserveDaemonWait(ctx, filepath.Dir(base), process)
	if err != nil {
		return err
	}
	if !owned && closed {
		return ErrBinding
	}
	if owned && !closed {
		return upgradePending("predecessor_wait_pending", nil)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return err
	}
	switch lifetime.CompareBootIDs(record.UpgradeGeneration.LegacyBootID, boot) {
	case lifetime.BootDifferent:
		return nil
	case lifetime.BootUnknown:
		return upgradePending("predecessor_boot_unknown", nil)
	case lifetime.BootSame:
		absent, err := c.privateProcessUnavailable(privateProcessIdentity{PID: record.LegacyPID, StartToken: record.LegacyProcessStart})
		if err != nil || !absent {
			return upgradePending("predecessor_process_pending", err)
		}
		return nil
	default:
		return ErrBinding
	}
}

type privateUpgradeStart struct {
	root   taskdir.RootPin
	record privateMigrationRecord
}

func (scope *privateUpgradeStart) authorizeStart(ctx context.Context, root string) error {
	if scope == nil || root != scope.root.Root() || !scope.record.LegacyStopped || !scope.record.UpgradeGeneration.AcceptedStart || scope.record.UpgradeGeneration.SuccessorPID != 0 {
		return ErrBinding
	}
	if err := validatePrivateUpgradeMigration(ctx, scope.root, scope.record); err != nil {
		return err
	}
	current, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found || current != scope.record {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func (c *Client) completePrivateUpgradeMigration(ctx context.Context, root, base string, pending privateMigrationRecord) error {
	pin, err := readControlRootPin(ctx, root)
	if err != nil {
		return err
	}
	if err = validatePrivateUpgradeMigration(ctx, pin, pending); err != nil {
		return err
	}
	if !pending.LegacyStopped {
		return upgradePending("predecessor_shutdown_pending", nil)
	}
	if !pending.UpgradeGeneration.AcceptedStart {
		pending, err = c.startPrivateUpgradeSuccessor(ctx, pin, base, pending)
		if err != nil {
			return err
		}
	}
	if pending.UpgradeGeneration.SuccessorPID == 0 {
		return upgradePending("successor_birth_unknown", ErrUnknown)
	}
	if err = c.verifyPrivateUpgradeSuccessor(ctx, root, pending); err != nil {
		return err
	}
	if !pending.UpgradeGeneration.Completed {
		completed := pending
		completed.UpgradeGeneration.Completed = true
		if err = replacePrivateMigrationRecordContext(ctx, base, pending, completed, c.options.PrivateGenerationGuard); err != nil {
			return err
		}
		pending = completed
	}
	if err = commitPrivateUpgradeCompletion(ctx, root, pending, c.options); err != nil {
		return err
	}
	return retirePrivateRecoveryBinding(ctx, root, c.Binding(), nil, c.options.PrivateGenerationGuard)
}

func (c *Client) startPrivateUpgradeSuccessor(ctx context.Context, root taskdir.RootPin, base string, pending privateMigrationRecord) (privateMigrationRecord, error) {
	if pending.UpgradeGeneration.AcceptedStart {
		return pending, upgradePending("successor_start_already_accepted", ErrUnknown)
	}
	if c.options.DaemonStartGuard == nil {
		return pending, ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitBirthOwner); !ok {
		return pending, ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitSettlementOwner); !ok {
		return pending, ErrInfrastructureOwnerRequired
	}
	if err := c.requirePrivateUpgradePreAcceptance(ctx, root, pending); err != nil {
		return pending, err
	}
	if err := c.preparePrivateDaemonStart(ctx, root.Root()); err != nil {
		return pending, fmt.Errorf("prepare accepted upgrade successor: %w", err)
	}
	accepted := pending
	accepted.UpgradeGeneration.AcceptedStart = true
	if err := replacePrivateMigrationRecordContext(ctx, base, pending, accepted, c.options.PrivateGenerationGuard); err != nil {
		return pending, c.settlePrivateUpgradeAcceptanceFault(context.WithoutCancel(ctx), root, pending, accepted, err)
	}
	scope := &privateUpgradeStart{root: root, record: accepted}
	started, startErr := startDaemonWithAuthorization(ctx, root.Root(), c.runtimeBinding.DaemonExecutable, c.binding.ConfigPath, base, c.options, scope)
	if started.done != nil {
		// The actual Start owns birth settlement even after caller cancellation.
		updated, birthErr := c.recordPrivateUpgradeBirth(context.WithoutCancel(ctx), base, accepted, started)
		return updated, errors.Join(startErr, birthErr)
	}
	if !started.attempted || started.notStarted {
		// This still-live caller knows the canonical Start callback was not
		// entered. Crash/unknown Start never receives this rollback permission.
		settleErr := c.releasePrivateUpgradeUnstarted(context.WithoutCancel(ctx), root, base, accepted, pending, started)
		return pending, errors.Join(startErr, settleErr)
	}
	return accepted, errors.Join(upgradePending("successor_birth_unknown", ErrUnknown), startErr)
}

func (c *Client) requirePrivateUpgradePreAcceptance(ctx context.Context, root taskdir.RootPin, expected privateMigrationRecord) error {
	if err := requirePrivateUpgradeAuthority(ctx, root, c.options); err != nil {
		return err
	}
	if !expected.LegacyStopped {
		return ErrBinding
	}
	if err := validatePrivateUpgradeMigration(ctx, root, expected); err != nil {
		return err
	}
	current, found, err := loadPrivateMigrationRecord(filepath.Join(root.Root(), privateSupervisorDirectory))
	if err != nil || !found || current != expected {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

// Only the still-live caller before canonical Start can enter this settlement.
// EX protects the exact pre-acceptance snapshot from another legitimate writer.
// Reading/resetting after guard return avoids nesting the resident commit lock.
func (c *Client) settlePrivateUpgradeAcceptanceFault(ctx context.Context, root taskdir.RootPin, before, accepted privateMigrationRecord, writeErr error) error {
	if err := requireRuntimeRestartBootstrapEX(ctx, root.Root()); err != nil {
		return errors.Join(writeErr, err)
	}
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return errors.Join(writeErr, err)
	}
	base := filepath.Join(root.Root(), privateSupervisorDirectory)
	winner, found, readErr := loadPrivateMigrationRecord(base)
	if readErr != nil || !found {
		return errors.Join(writeErr, readErr, ErrBinding)
	}
	if winner == before {
		return writeErr
	}
	if winner != accepted {
		return errors.Join(writeErr, ErrBinding)
	}
	// Known for this exact live path: the canonical starter was never invoked.
	neverInvoked := privateDaemonStart{}
	rollbackErr := c.releasePrivateUpgradeUnstarted(ctx, root, base, accepted, before, neverInvoked)
	return errors.Join(writeErr, rollbackErr)
}

// This rollback consumes only live proof that canonical Start was not invoked
// or definitively did not launch. Generic progress replacement cannot remove
// acceptance, including after a crash.
func (c *Client) releasePrivateUpgradeUnstarted(ctx context.Context, root taskdir.RootPin, base string, expected, replacement privateMigrationRecord, started privateDaemonStart) error {
	if started.done != nil || started.attempted && !started.notStarted {
		return ErrBinding
	}
	accepted := replacement
	accepted.UpgradeGeneration.AcceptedStart = true
	if accepted != expected {
		return ErrBinding
	}
	return guardPrivateGeneration(ctx, root.Root(), c.options.PrivateGenerationGuard, func() error {
		if err := validatePrivateUpgradeMigration(ctx, root, expected); err != nil {
			return err
		}
		writeErr := replacePrivateMigrationRecord(base, expected, replacement)
		return settlePrivateUpgradeMigrationWrite(base, expected, replacement, writeErr)
	})
}

func (c *Client) recordPrivateUpgradeBirth(ctx context.Context, base string, pending privateMigrationRecord, started privateDaemonStart) (privateMigrationRecord, error) {
	if started.birthReady == nil || started.birth == nil {
		return pending, upgradePending("successor_birth_unknown", ErrUnknown)
	}
	select {
	case <-started.birthReady:
	case <-ctx.Done():
		return pending, upgradePending("successor_birth_unknown", ctx.Err())
	}
	birth, ready, err := started.birth()
	if err != nil || !ready {
		return pending, upgradePending("successor_birth_unknown", errors.Join(ErrUnknown, err))
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return pending, err
	}
	updated := pending
	updated.UpgradeGeneration.SuccessorPID = birth.PID()
	updated.UpgradeGeneration.SuccessorProcessStart = birth.StartToken()
	updated.UpgradeGeneration.SuccessorBootID = boot
	if err = replacePrivateMigrationRecordContext(ctx, base, pending, updated, c.options.PrivateGenerationGuard); err != nil {
		return pending, err
	}
	return updated, nil
}

func (c *Client) verifyPrivateUpgradeSuccessor(ctx context.Context, root string, record privateMigrationRecord) error {
	birth, err := c.privateUpgradeSuccessorBirth(ctx, root, record)
	if err != nil {
		return err
	}
	if err = waitPrivateReadyWithProbe(ctx, c, c.options.ObservationTimeout, false, c.runtimeRestartReadiness); err != nil {
		return err
	}
	pid, found, err := c.privateDaemonPID()
	if err != nil || !found || pid != birth.PID() {
		return upgradePending("successor_pid_mismatch", err)
	}
	present, err := lifetime.ProcessPresent(birth)
	if err != nil || !present {
		return upgradePending("successor_not_ready", err)
	}
	return nil
}

func (c *Client) privateUpgradeSuccessorBirth(ctx context.Context, root string, record privateMigrationRecord) (lifetime.ProcessRef, error) {
	u := record.UpgradeGeneration
	if !record.LegacyStopped || !u.AcceptedStart || u.SuccessorBootID == "" {
		return lifetime.ProcessRef{}, ErrBinding
	}
	boot, err := lifetime.BootID()
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	if lifetime.CompareBootIDs(u.SuccessorBootID, boot) != lifetime.BootSame {
		return lifetime.ProcessRef{}, upgradePending("successor_boot_changed", nil)
	}
	birth, err := lifetime.NewProcessRef(u.SuccessorPID, u.SuccessorProcessStart)
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitSettlementOwner)
	if !ok {
		return lifetime.ProcessRef{}, ErrInfrastructureOwnerRequired
	}
	owned, closed, err := owner.ObserveDaemonWait(ctx, root, birth)
	if err != nil {
		return lifetime.ProcessRef{}, err
	}
	if !owned && closed {
		return lifetime.ProcessRef{}, ErrBinding
	}
	if closed {
		return lifetime.ProcessRef{}, upgradePending("successor_wait_closed", nil)
	}
	return birth, nil
}

func commitPrivateUpgradeCompletion(ctx context.Context, root string, pending privateMigrationRecord, options Options) error {
	if !pending.UpgradeGeneration.Completed {
		return ErrBinding
	}
	pin, err := readControlRootPin(ctx, root)
	if err != nil {
		return err
	}
	if err = validatePrivateUpgradeMigration(ctx, pin, pending); err != nil {
		return err
	}
	drain, found, err := readPrivateUpgrade(pin, pending.UpgradeGeneration.OperationID)
	if err != nil || !found || drain.Phase != UpgradeMigrating {
		return errors.Join(ErrBinding, err)
	}
	return guardPrivateGeneration(ctx, root, options.PrivateGenerationGuard, func() error {
		return commitPrivateUpgradePublicationWithinGuard(ctx, pin, drain, pending)
	})
}

func commitPrivateUpgradePublicationWithinGuard(ctx context.Context, pin taskdir.RootPin, drain privateUpgradeDrain, pending privateMigrationRecord) error {
	if err := validatePrivateUpgradeMigration(ctx, pin, pending); err != nil {
		return err
	}
	root := pin.Root()
	base := filepath.Join(root, privateSupervisorDirectory)
	current, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found || current != pending {
		return errors.Join(ErrBinding, err)
	}
	publication := privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: pending.UpgradeGeneration.Target}
	data, err := task.MarshalCanonical(publication)
	if err != nil {
		return err
	}
	if err = publishPrivateRecord(base, privateIdentityPublicationPath(root), data); err != nil {
		return err
	}
	if err = publishPrivateIdentityPublicationValidated(root, publication); err != nil {
		return err
	}
	if err = appendPrivateMigrationHistory(base, pending); err != nil {
		return err
	}
	if err = clearPrivateMigrationRecord(base, pending); err != nil {
		return err
	}
	return clearPrivateUpgradeWithinCommit(ctx, pin, drain)
}

// clearPrivateUpgradeWithinCommit is used only after native completion and
// completed history are durable. Generic startup and identity clear never call it.
func clearPrivateUpgradeWithinCommit(ctx context.Context, root taskdir.RootPin, expected privateUpgradeDrain) error {
	if err := requireRuntimeRestartBootstrapEX(ctx, root.Root()); err != nil {
		return err
	}
	if err := checkPrivateUpgradeRoot(ctx, root); err != nil {
		return err
	}
	current, found, err := readPrivateUpgrade(root, expected.OperationID)
	if err != nil || !found || current != expected {
		return errors.Join(ErrBinding, err)
	}
	if err = os.Remove(privateBootstrapRecordPath(root.Root())); err != nil {
		return fmt.Errorf("%w: clear completed upgrade fence: %w", ErrConfiguration, err)
	}
	return syncPrivateDirectory(filepath.Join(root.Root(), privateSupervisorDirectory))
}
