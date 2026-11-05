package supervisor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ExecuteReferencedPrivateMigration borrows the caller's actual pinned Store.
// The canonical migration journal owns phases/history; this consumer only
// validates retained target content and resolves the selected control context.
// The caller supplies native guard/Wait authority and retains its root admission
// fence. Contention is one-shot: a handler may observe the existing owner under
// its original deadline, but this consumer never selects or retries a migration.
func ExecuteReferencedPrivateMigration(ctx context.Context, store *taskdir.Store, reference infrastructure.MigrationReference, options pueue.Options) (pueue.PrivateMigrationResult, error) {
	if ctx == nil || store == nil {
		return "", os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := reference.Validate(); err != nil {
		return "", err
	}
	if reference.HistoryIndex > uint64(math.MaxInt) {
		return "", fmt.Errorf("%w: migration history index exceeds native int", infrastructure.ErrIdentity)
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return "", err
	}
	if pin.RootID() != reference.RootID {
		return "", task.ErrIdentityMismatch
	}
	options, err = referencedMigrationOptions(options)
	if err != nil {
		return "", err
	}
	ref := pueue.PrivateMigrationReference{RootID: reference.RootID, RecordSHA256: reference.RecordSHA256, HistoryIndex: int(reference.HistoryIndex)}
	resolver := retainedMigrationTargetResolver{store: store, pin: pin, options: options}
	var result pueue.PrivateMigrationResult
	acquired, err := pueue.TryWithPrivateTransition(ctx, pin.Root(), func() error {
		return pueue.WithExistingBootstrapLockContext(ctx, pin.Root(), func(locked context.Context) error {
			if pinErr := checkInfrastructureTarget(locked, store, pin); pinErr != nil {
				return pinErr
			}
			var executeErr error
			result, executeErr = pueue.ExecuteReferencedPrivateMigrationWithLock(locked, pin.Root(), ref, resolver, options)
			return errors.Join(executeErr, checkInfrastructureTarget(locked, store, pin))
		})
	})
	if err != nil {
		return result, err
	}
	if !acquired {
		return "", fmt.Errorf("%w: private supervisor transition is owned", task.ErrLockBusy)
	}
	return result, nil
}

// This referenced boundary requires both accepted snapshots explicitly; legacy
// Options normalization and its inheritance semantics remain owned by Pueue.
func referencedMigrationOptions(options pueue.Options) (pueue.Options, error) {
	if options.Environment == nil || options.PrivateMaintenanceEnvironment == nil || options.DaemonStartGuard == nil || options.DaemonWaitOwner == nil {
		return pueue.Options{}, os.ErrInvalid
	}
	for _, snapshot := range [][]string{options.Environment, options.PrivateMaintenanceEnvironment} {
		if err := task.ValidateEnvironment(snapshot); err != nil {
			return pueue.Options{}, errors.Join(pueue.ErrConfiguration, err)
		}
	}
	options.Environment = slices.Clone(options.Environment)
	options.PrivateMaintenanceEnvironment = slices.Clone(options.PrivateMaintenanceEnvironment)
	// Accepted selected environment owns fresh target resolution. Cached X
	// resolution is not authority for recorded Y; Bind derives it read-only.
	options.Resolution = nil
	// This operation retains its borrowed root and generation until every
	// finite control finishes, including a Bind returning client plus error.
	options.WaitForPendingClose = true
	return options, nil
}

type retainedMigrationTargetResolver struct {
	store   *taskdir.Store
	pin     taskdir.RootPin
	options pueue.Options
}

func (r retainedMigrationTargetResolver) ResolvePrivateMigrationTarget(ctx context.Context, root string, identity pueue.PrivateMigrationIdentity) (target task.SupervisorRef, resultErr error) {
	if root != r.pin.Root() {
		return task.SupervisorRef{}, task.ErrIdentityMismatch
	}
	if !pueue.IsPrivateConfig(root, identity.ConfigPath) {
		return task.SupervisorRef{}, pueue.ErrBinding
	}
	if err := checkInfrastructureTarget(ctx, r.store, r.pin); err != nil {
		return task.SupervisorRef{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, checkInfrastructureTarget(ctx, r.store, r.pin))
		if resultErr != nil {
			target = task.SupervisorRef{}
		}
	}()
	base := filepath.Join(root, ".supervisor")
	pair, candidate, err := retainedReferencedMigrationTarget(base, identity)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	client, err := pueue.Bind(ctx, candidate.ClientExecutable, identity.ConfigPath, r.options)
	defer func() { resultErr = errors.Join(resultErr, client.CloseAfterPending(resultErr)) }()
	if err != nil {
		return task.SupervisorRef{}, err
	}
	target = client.Binding()
	if target.ClientExecutable != candidate.ClientExecutable || target.ClientSHA256 != candidate.ClientSHA256 || !task.SameSupervisorIdentity(target, candidate) {
		return task.SupervisorRef{}, pueue.ErrBinding
	}
	target.DaemonExecutable, target.DaemonSHA256 = identity.Executable, identity.SHA256
	if err = task.ValidateFreshSupervisorRef(target); err != nil {
		return task.SupervisorRef{}, err
	}
	// Bind ran native finite control. Revalidate both retained members and the
	// committed directory name afterward; never reread a mutable pair pointer.
	if err = validateSavedStateSupervisorPairContents(base, pair, target); err != nil {
		return task.SupervisorRef{}, err
	}
	return target, nil
}

func retainedReferencedMigrationTarget(base string, identity pueue.PrivateMigrationIdentity) (string, task.SupervisorRef, error) {
	pair, managed := stateSupervisorPairDirectoryForExecutable(base, identity.Executable)
	if !managed || filepath.Base(identity.Executable) != "pueued" {
		return "", task.SupervisorRef{}, pueue.ErrReferencedMigrationUnsupported
	}
	client := filepath.Join(pair, "pueue")
	digest, err := protectedStateExecutableDigest(client)
	if err != nil {
		return "", task.SupervisorRef{}, err
	}
	candidate := task.SupervisorRef{
		ClientExecutable: client, ClientSHA256: digest,
		DaemonExecutable: identity.Executable, DaemonSHA256: identity.SHA256,
		ConfigPath: identity.ConfigPath, ConfigDigest: identity.ConfigDigest,
		ResolvedConfigSHA256: identity.ResolvedConfigSHA256, Endpoint: identity.Endpoint,
	}
	if err = validateSavedStateSupervisorPairContents(base, pair, candidate); err != nil {
		return "", task.SupervisorRef{}, err
	}
	return pair, candidate, nil
}
