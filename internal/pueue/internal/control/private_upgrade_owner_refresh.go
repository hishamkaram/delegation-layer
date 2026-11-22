package control

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObservePrivateUpgradeAcceptedStartWithLock reads the canonical current or
// exact upgrade and its unfinished accepted successor Start. The caller owns
// retirement policy. An unfinished Start remains accepted when birth is unknown.
// The actual bootstrap EX keeps these reads in one canonical transition scope.
func ObservePrivateUpgradeAcceptedStartWithLock(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID) (observed PrivateUpgradeObservation, accepted bool, resultErr error) {
	if err := requireRuntimeRestartBootstrapEX(ctx, root.Root()); err != nil {
		return observed, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, checkPrivateUpgradeRoot(ctx, root)) }()
	var found bool
	observed, found, resultErr = ObservePrivateUpgrade(ctx, root, operation)
	if resultErr != nil || observed.Completed {
		return observed, false, resultErr
	}
	if operation != "" && !found {
		return observed, false, ErrPrivateUpgradeNotRetained
	}
	pending, present, err := loadPrivateMigrationRecord(filepath.Join(root.Root(), privateSupervisorDirectory))
	if err != nil {
		return observed, false, err
	}
	if !found {
		// A valid ordinary migration is outside the upgrade fact. The loader
		// rejects an upgrade migration without its matching canonical drain.
		return observed, false, nil
	}
	if !present {
		if observed.Phase == UpgradeMigrating {
			return observed, false, requirePrivateUpgradeRecordedCompletion(ctx, root, observed.OperationID)
		}
		return observed, false, nil
	}
	if err := validatePrivateUpgradeMigration(ctx, root, pending); err != nil {
		return observed, false, err
	}
	if pending.UpgradeGeneration.Completed {
		return observed, false, requirePrivateUpgradePublishedCompletion(root, pending)
	}
	return observed, pending.UpgradeGeneration.AcceptedStart, nil
}

// Publication may commit history before removing migration and clearing Drain.
// Only the existing Complete owner may perform that final cleanup; this read
// proves the exact published completion without synthesizing Completed=true.
func requirePrivateUpgradeRecordedCompletion(ctx context.Context, root taskdir.RootPin, operation UpgradeOperationID) error {
	base := filepath.Join(root.Root(), privateSupervisorDirectory)
	history, _, err := loadPrivateMigrationHistory(base)
	if err != nil {
		return err
	}
	for _, record := range history.Migrations {
		if record.UpgradeGeneration.OperationID != operation {
			continue
		}
		if err := validatePrivateUpgradeMigration(ctx, root, record); err != nil {
			return err
		}
		return requirePrivateUpgradePublishedCompletion(root, record)
	}
	return ErrBinding
}

func requirePrivateUpgradePublishedCompletion(root taskdir.RootPin, record privateMigrationRecord) error {
	base := filepath.Join(root.Root(), privateSupervisorDirectory)
	complete, err := privateMigrationCompletionRecorded(root.Root(), base, record.UpgradeGeneration.Target, record)
	if err != nil || !complete {
		return errors.Join(ErrBinding, err)
	}
	return nil
}
