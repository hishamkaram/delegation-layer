package control

import (
	"context"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func guardPrivateGeneration(ctx context.Context, root string, guard PrivateGenerationGuard, commit func() error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard == nil {
		return ErrInfrastructureOwnerRequired
	}
	return guard.GuardPrivateGenerationCommit(ctx, root, commit)
}

// The unguarded canonical publisher also serves exact pending-body replay and
// migration completion's larger transaction. Never call a guarded wrapper from
// a commit callback: the resident authority's commit lock is non-reentrant.
func publishPrivateDaemonIdentityContext(ctx context.Context, base string, binding task.SupervisorRef, guard PrivateGenerationGuard) error {
	return guardPrivateGeneration(ctx, filepath.Dir(base), guard, func() error {
		return commitPrivateDaemonIdentity(base, binding)
	})
}

func commitPrivateDaemonIdentity(base string, binding task.SupervisorRef) error {
	if err := publishPrivateDaemonIdentity(base, binding); err != nil {
		return err
	}
	if root, private := privateStateRoot(binding.ConfigPath); private {
		return clearPrivateBootstrapPending(root)
	}
	return nil
}

func publishPrivateMigrationStateContext(ctx context.Context, base string, record privateMigrationRecord, guard PrivateGenerationGuard) error {
	current, found, err := loadPrivateMigrationRecord(base)
	if err != nil {
		return err
	}
	if found && current == record {
		return publishPrivateMigrationState(base, record)
	}
	return guardPrivateGeneration(ctx, filepath.Dir(base), guard, func() error {
		return publishPrivateMigrationState(base, record)
	})
}

func replacePrivateMigrationRecordContext(ctx context.Context, base string, expected, replacement privateMigrationRecord, guard PrivateGenerationGuard) error {
	return guardPrivateGeneration(ctx, filepath.Dir(base), guard, func() error {
		return replacePrivateMigrationRecord(base, expected, replacement)
	})
}

func replaceReconciledPrivateBindingContext(ctx context.Context, root string, before, candidate task.SupervisorRef, guard PrivateGenerationGuard) error {
	return guardPrivateGeneration(ctx, root, guard, func() error {
		return replaceReconciledPrivateBinding(root, before, candidate)
	})
}

// The completed edge and exact published successor jointly authorize replay;
// a pending identity body by itself does not encode migration completion.
func privateMigrationCompletionRecorded(root, base string, binding task.SupervisorRef, pending privateMigrationRecord) (bool, error) {
	history, _, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return false, err
	}
	for _, existing := range history.Migrations {
		if existing != pending {
			continue
		}
		published, found, err := loadPrivateSupervisorBinding(root)
		if err != nil || !found || published != binding {
			return false, err
		}
		identity, found, err := loadPrivateDaemonIdentity(base)
		if err != nil || !found || identity != privateDaemonIdentityForBinding(binding) {
			return false, err
		}
		_, publishing, err := readPrivateIdentityPublication(root)
		return !publishing, err
	}
	return false, nil
}
