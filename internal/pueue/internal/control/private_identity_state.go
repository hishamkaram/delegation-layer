package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	return privateDaemonIdentityError(migration.ValidateDaemonIdentity(identity))
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

// ReadPrivateSupervisorBinding returns the validated active generation for a
// Delegate-owned private root. It never repairs, migrates, or starts a daemon;
// callers must still use Client operations, which revalidate the generation
// under the appropriate supervisor lease before issuing controls.
func ReadPrivateSupervisorBinding(stateRoot string) (task.SupervisorRef, bool, error) {
	return loadPrivateSupervisorBinding(stateRoot)
}

// publishPrivateSupervisorBindingIfAbsent records a verified predecessor
// before migration can stop its daemon. It is intentionally create-once: a
// later generation may replace the logical binding only through the
// readiness-gated identity publication transaction.
func publishPrivateSupervisorBindingIfAbsent(stateRoot string, binding task.SupervisorRef) error {
	return publishPrivateSupervisorBindingIfAbsentContext(context.Background(), stateRoot, binding)
}

func publishPrivateSupervisorBindingIfAbsentContext(ctx context.Context, stateRoot string, binding task.SupervisorRef) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
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
	// Cancellation can revoke publication before staging. Once staging starts,
	// the canonical writer retains ownership through its barriers and cleanup.
	if err = ctx.Err(); err != nil {
		return err
	}
	return publishPrivateRecord(filepath.Join(stateRoot, privateSupervisorDirectory), privateSupervisorBindingPath(stateRoot), data)
}

// PublishPrivateSupervisorBindingIfAbsent records a validated logical owner
// when an older state root has daemon provenance but no separate binding file.
// It never replaces an existing generation.
func PublishPrivateSupervisorBindingIfAbsent(stateRoot string, binding task.SupervisorRef) error {
	return publishPrivateSupervisorBindingIfAbsent(stateRoot, binding)
}

// PublishPrivateSupervisorBindingIfAbsentContext preserves caller cancellation
// before the canonical create-once publisher starts staging the binding.
func PublishPrivateSupervisorBindingIfAbsentContext(ctx context.Context, stateRoot string, binding task.SupervisorRef) error {
	return publishPrivateSupervisorBindingIfAbsentContext(ctx, stateRoot, binding)
}

// EnsurePrivateSupervisorBindingWithLock adopts a live private daemon whose
// launch identity predates the separate logical binding record. The caller
// holds the root bootstrap lock. Existing identities are never replaced, and
// success requires the canonical private lifecycle to be complete.
func (c *Client) EnsurePrivateSupervisorBindingWithLock(ctx context.Context, stateRoot string) error {
	return c.ensurePrivateSupervisorBindingWithReady(ctx, stateRoot, c.Ready)
}

// ready is a scoped test seam for cancellation after a completed observation.
// Production always uses Ready and the same create-once publication owner.
func (c *Client) ensurePrivateSupervisorBindingWithReady(ctx context.Context, stateRoot string, ready func(context.Context) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	if c.binding != c.runtimeBinding {
		return ErrPrivateSupervisorMigrationRequired
	}
	if err := ready(ctx); err != nil {
		return privatePublishedRecoveryError(err)
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return privatePublishedRecoveryError(err)
	}
	if !found || !privateDaemonIdentityMatchesBinding(identity, c.binding) {
		return fmt.Errorf("%w: private daemon identity does not match the live binding", ErrPrivateSupervisorMigrationRequired)
	}
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return privatePublishedRecoveryError(err)
	}
	if found && !privateFullGenerationMatches(published, c.binding) {
		return fmt.Errorf("%w: private supervisor is not the published generation", ErrPrivateSupervisorMigrationRequired)
	}
	if err = publishPrivateSupervisorBindingIfAbsentContext(ctx, stateRoot, c.binding); err != nil {
		return privatePublishedRecoveryError(err)
	}
	// Binding publication has already run. Preserve the migration postcondition
	// error without claiming that this operation stopped before mutation.
	incompleteErr := fmt.Errorf("%w: published private supervisor transition is incomplete", ErrPrivateSupervisorMigrationRequired)
	return ensurePrivateSupervisorMigrationComplete(stateRoot, c.binding, incompleteErr)
}

func ensurePrivateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef, incompleteErr error) error {
	// A missing caller-supplied failure must never authorize incomplete state.
	if incompleteErr == nil {
		return ErrBinding
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, binding)
	if err != nil {
		return privatePublishedRecoveryError(err)
	}
	if !complete {
		return incompleteErr
	}
	return nil
}

// authorizePrivateMutationWithLock checks the published generation while the
// caller retains its EX lease. A compatible recovery pair can control that
// generation without owning its persisted executable provenance.
func (c *Client) authorizePrivateMutationWithLock(stateRoot string) error {
	published, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		return privatePublishedRecoveryError(err)
	}
	if !found {
		return ErrPrivateSupervisorMigrationRequired
	}
	if err = ensurePrivateSupervisorMigrationComplete(stateRoot, published, ErrPrivateSupervisorTransitionIncomplete); err != nil {
		return err
	}
	if !privateBindingsMatch(c.runtimeBinding, published) {
		return ErrBinding
	}
	return nil
}

func restorePrivateSupervisorBindingFromSaved(ctx context.Context, stateRoot string, binding task.SupervisorRef) error {
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
	return publishPrivateSupervisorBindingIfAbsentContext(ctx, stateRoot, binding)
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
	return migration.LogicalIdentityPresent(identity)
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
