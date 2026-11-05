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

// PrivateMigrationReference names a state result, not a unique launch attempt.
// HistoryIndex is the first matching immutable projection, or the history length
// when the pending projection has not yet been completed.
type PrivateMigrationReference struct {
	RootID       string
	RecordSHA256 string
	HistoryIndex int
}

// PrivateMigrationIdentity is the canonical journal's immutable target data.
// It grants no authority to select an executable or start a daemon.
type PrivateMigrationIdentity = migration.DaemonIdentity

// PrivateMigrationTargetResolver resolves only a validated canonical target.
// The provider owns retained-pair validation and accepted context preparation.
// Unsupported historical targets must fail before any migration mutation.
type PrivateMigrationTargetResolver interface {
	ResolvePrivateMigrationTarget(context.Context, string, PrivateMigrationIdentity) (task.SupervisorRef, error)
}

type PrivateMigrationResult string

const (
	// PrivateMigrationCompleted reports canonical completion, not a new Start or
	// a claim that the caller's selected context was applied.
	PrivateMigrationCompleted PrivateMigrationResult = "completed"
	// PrivateMigrationWarmReuse reports an already-current ready target. The
	// requested context was not applied.
	PrivateMigrationWarmReuse PrivateMigrationResult = "warm_reuse"
)

var (
	ErrReferencedMigrationUnsupported    = errors.New("unsupported referenced private migration target")
	ErrReferencedMigrationTargetNotReady = errors.New("referenced private migration target is current but not ready")
)

type privateMigrationProjection struct {
	SchemaVersion int                   `json:"schema_version"`
	Legacy        privateDaemonIdentity `json:"legacy"`
	Successor     privateDaemonIdentity `json:"successor"`
}

func privateMigrationProjectionSHA(record privateMigrationRecord) (string, error) {
	data, err := task.MarshalCanonical(privateMigrationProjection{record.SchemaVersion, record.Legacy, record.Successor})
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func firstPrivateMigrationProjection(history privateMigrationHistory, digest string) (int, error) {
	for index, record := range history.Migrations {
		candidate, err := privateMigrationProjectionSHA(record)
		if err != nil {
			return 0, err
		}
		if candidate == digest {
			return index, nil
		}
	}
	return len(history.Migrations), nil
}

// ReadPrivateMigrationReferenceWithLock captures the pending state result under
// the caller's generation/bootstrap serialization and actual native root pin.
// It writes nothing and can rederive the same reference after append-before-clear.
func ReadPrivateMigrationReferenceWithLock(root string) (PrivateMigrationReference, bool, error) {
	rootID, err := privateMigrationRootID(root)
	if err != nil {
		return PrivateMigrationReference{}, false, err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	pending, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found {
		return PrivateMigrationReference{}, false, err
	}
	digest, err := privateMigrationProjectionSHA(pending)
	if err != nil {
		return PrivateMigrationReference{}, false, err
	}
	history, _, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return PrivateMigrationReference{}, false, err
	}
	index, err := firstPrivateMigrationProjection(history, digest)
	if err != nil {
		return PrivateMigrationReference{}, false, err
	}
	return PrivateMigrationReference{rootID, digest, index}, true, nil
}

func privateMigrationRootID(root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", ErrConfiguration
	}
	if err := rejectSymlinkComponents(root); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ErrConfiguration
	}
	data, err := readRegular(filepath.Join(root, "root.json"), MaxControlBytes)
	if err != nil {
		return "", err
	}
	var record task.RootRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return "", err
	}
	if err = task.ValidateRootRecord(&record); err != nil {
		return "", err
	}
	return record.RootID, nil
}

// ExecuteReferencedPrivateMigrationWithLock consumes exactly one recorded state
// result. The caller retains the native pin and generation/bootstrap locks and
// supplies its actual Start guard and Wait owner. Neither the reference nor the
// resolver confers those authorities. Ordinary migration selection is bypassed.
func ExecuteReferencedPrivateMigrationWithLock(ctx context.Context, root string, ref PrivateMigrationReference, resolver PrivateMigrationTargetResolver, options Options) (result PrivateMigrationResult, resultErr error) {
	if ctx == nil {
		return "", ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	record, pending, err := referencedPrivateMigrationRecord(root, ref)
	if err != nil {
		return "", err
	}
	if resolver == nil || !privateLogicalIdentityPresent(record.Successor) {
		return "", ErrReferencedMigrationUnsupported
	}
	target, err := resolver.ResolvePrivateMigrationTarget(ctx, root, record.Successor)
	if err != nil {
		return "", err
	}
	if !privateDaemonIdentityMatchesBinding(record.Successor, target) || !IsPrivateConfig(root, target.ConfigPath) {
		return "", fmt.Errorf("%w: resolved target differs from recorded successor", ErrPrivateSupervisorMigrationRequired)
	}
	client, err := prepareExactPublishedPair(ctx, target, options)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	if !pending {
		if err = client.verifyReferencedPrivateCompletion(ctx, root, ref); err != nil {
			return "", err
		}
		return PrivateMigrationWarmReuse, nil
	}
	if err = client.resumeReferencedPrivateMigration(ctx, root, record); err != nil {
		return "", err
	}
	if err = client.verifyReferencedPrivateCompletion(ctx, root, ref); err != nil {
		return "", err
	}
	return PrivateMigrationCompleted, nil
}

func referencedPrivateMigrationRecord(root string, ref PrivateMigrationReference) (privateMigrationRecord, bool, error) {
	if err := validatePrivateMigrationReferenceRoot(root, ref); err != nil {
		return privateMigrationRecord{}, false, err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	record, pending, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return privateMigrationRecord{}, false, err
	}
	// A different pending target always refuses, before any historical shortcut.
	if pending {
		digest, digestErr := privateMigrationProjectionSHA(record)
		if digestErr != nil {
			return privateMigrationRecord{}, false, digestErr
		}
		if digest != ref.RecordSHA256 {
			return privateMigrationRecord{}, false, ErrPrivateSupervisorMigrationRequired
		}
	}
	history, _, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return privateMigrationRecord{}, false, err
	}
	if err = validateReferencedPrivateMigrationSlot(history, ref, pending); err != nil {
		return privateMigrationRecord{}, false, err
	}
	if !pending {
		record = history.Migrations[ref.HistoryIndex]
	}
	return record, pending, nil
}

func validatePrivateMigrationReferenceRoot(root string, ref PrivateMigrationReference) error {
	if ref.HistoryIndex < 0 {
		return ErrBinding
	}
	if err := errors.Join(task.ValidateRootID(ref.RootID), task.ValidateSHA256(ref.RecordSHA256)); err != nil {
		return err
	}
	rootID, err := privateMigrationRootID(root)
	if err != nil {
		return err
	}
	if rootID != ref.RootID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateReferencedPrivateMigrationSlot(history privateMigrationHistory, ref PrivateMigrationReference, pending bool) error {
	if ref.HistoryIndex > len(history.Migrations) || (!pending && ref.HistoryIndex == len(history.Migrations)) {
		return ErrPrivateSupervisorMigrationRequired
	}
	if ref.HistoryIndex < len(history.Migrations) {
		digest, err := privateMigrationProjectionSHA(history.Migrations[ref.HistoryIndex])
		if err != nil {
			return err
		}
		if digest != ref.RecordSHA256 {
			return ErrPrivateSupervisorMigrationRequired
		}
	}
	// Validate canonical FIRST-slot representation, never redirect to another
	// slot when the exact referenced slot is absent or conflicts.
	for _, earlier := range history.Migrations[:ref.HistoryIndex] {
		digest, err := privateMigrationProjectionSHA(earlier)
		if err != nil {
			return err
		}
		if digest == ref.RecordSHA256 {
			return ErrPrivateSupervisorMigrationRequired
		}
	}
	return nil
}

func (c *Client) resumeReferencedPrivateMigration(ctx context.Context, root string, pending privateMigrationRecord) error {
	if _, err := recoverPrivateIdentityPublication(root); err != nil {
		return err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	identity, found, err := loadPrivateDaemonIdentity(base)
	if err != nil {
		return err
	}
	binding, bindingFound, err := loadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !bindingFound {
		binding, err = c.resolveMissingMigrationBinding(ctx, root, identity)
		if err != nil {
			return err
		}
		if err = publishPrivateSupervisorBindingIfAbsentContext(ctx, root, binding, c.options.PrivateGenerationGuard); err != nil {
			return err
		}
		bindingFound = true
	}
	return c.resumeReferencedPrivateMigrationPhase(ctx, root, binding, bindingFound, identity, found, pending)
}

// This edge can only finish the recorded successor. In particular it cannot
// enter ordinary migration's finish-recorded-then-select-another control flow.
func (c *Client) resumeReferencedPrivateMigrationPhase(ctx context.Context, root string, binding task.SupervisorRef, bindingFound bool, identity privateDaemonIdentity, found bool, pending privateMigrationRecord) error {
	current := privateDaemonIdentity{Executable: c.runtimeBinding.DaemonExecutable, SHA256: c.runtimeBinding.DaemonSHA256}
	logical := privateDaemonIdentityForBinding(c.binding)
	if err := validatePendingPrivateMigration(pending, binding, bindingFound, c.binding, current, logical); err != nil {
		return err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	if !pending.LegacyStopped && !pending.LegacyShutdownDispatched && !pending.LegacyShutdownRequested {
		return c.resumeValidatedPreparedPrivateMigration(ctx, root, base, binding, bindingFound, identity, found, current, logical, pending)
	}
	stopped, err := c.provePrivateMigrationShutdown(ctx, base, binding, identity, found, pending)
	if err != nil {
		return err
	}
	return c.completePendingPrivateMigration(ctx, root, base, stopped)
}

func sameFullPrivateMigrationTarget(current, target task.SupervisorRef) bool {
	return task.SameSupervisorIdentity(current, target) && current.ClientExecutable == target.ClientExecutable && current.ClientSHA256 == target.ClientSHA256 && current.DaemonExecutable == target.DaemonExecutable && current.DaemonSHA256 == target.DaemonSHA256
}

func (c *Client) verifyReferencedPrivateCompletion(ctx context.Context, root string, ref PrivateMigrationReference) error {
	_, pending, err := referencedPrivateMigrationRecord(root, ref)
	if err != nil {
		return err
	}
	if pending {
		return ErrPrivateSupervisorMigrationRequired
	}
	binding, found, err := loadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !found || !sameFullPrivateMigrationTarget(binding, c.Binding()) {
		return ErrPrivateSupervisorMigrationRequired
	}
	observation, err := c.availableForTransition(ctx, true)
	if err != nil {
		return err
	}
	if !observation.ready() {
		return ErrReferencedMigrationTargetNotReady
	}
	return nil
}
