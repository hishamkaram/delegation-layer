package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func repairQueuedRunnerCommandsToTarget(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, targetRunner string) error {
	replacements, err := collectQueuedRunnerCommandReplacements(ctx, store, supervisor, targetRunner)
	if err != nil || len(replacements) == 0 {
		return err
	}
	return supervisor.ReplaceQueuedRunnerCommands(ctx, replacements)
}

func PlanQueuedRunnerRepairTarget(runnerExecutable string) (RunnerRepairTarget, error) {
	runner, err := ResolveRunner(runnerExecutable, "")
	if err != nil {
		return RunnerRepairTarget{}, err
	}
	if pueue.IsLegacyStateRunnerPath(filepath.Dir(filepath.Dir(runner)), runner) {
		return RunnerRepairTarget{}, fmt.Errorf("%w: historical runner copy is not an installed role", pueue.ErrConfiguration)
	}
	return RunnerRepairTarget{executable: runner}, nil
}

// ValidateExecutable checks the resolver's selected installed path and current
// executable metadata. Ownership and immutable command identity are separate.
func (target RunnerRepairTarget) ValidateExecutable(path string) error {
	if target.executable == "" || path != target.executable {
		return task.ErrIdentityMismatch
	}
	_, err := ResolveRunner(path, "")
	return err
}

func collectQueuedRunnerCommandReplacements(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, targetRunner string) ([]pueue.RunnerCommandReplacement, error) {
	snapshot, err := supervisor.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	prefix := "delegate:" + store.RootID + ":"
	replacements := make([]pueue.RunnerCommandReplacement, 0)
	for _, job := range snapshot.Jobs {
		if job.State != pueue.StateQueued && job.State != pueue.StateStashed {
			continue
		}
		replacement, eligible, prepareErr := prepareQueuedRunnerCommandForJob(ctx, store, supervisor, job, prefix)
		if prepareErr != nil {
			return nil, prepareErr
		}
		if !eligible {
			continue
		}
		replacements = append(replacements, replacement)
	}
	if len(replacements) == 0 {
		return nil, nil
	}
	previousRunners, err := recordedRepairRunnerPaths(ctx, store)
	if err != nil {
		return nil, err
	}
	for i := range replacements {
		replacements[i].NewRunner = targetRunner
		replacements[i].PreviousManagedRunners = previousRunners
	}
	if err = authorizeQueuedInspectionRunnerUpgrades(ctx, store, replacements); err != nil {
		return nil, err
	}
	if err = recordQueuedManagedRunnerMigrations(ctx, store, replacements); err != nil {
		return nil, err
	}
	return replacements, nil
}

func recordQueuedManagedRunnerMigrations(ctx context.Context, store *taskdir.Store, replacements []pueue.RunnerCommandReplacement) error {
	if store == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	for _, replacement := range replacements {
		if replacement.Inspection {
			continue
		}
		td, err := store.OpenTask(replacement.TaskID)
		if err != nil {
			return err
		}
		_, meta, readErr := td.PreparedRecords()
		if readErr == nil && meta == nil {
			readErr = task.ErrEvidenceFault
		}
		if readErr == nil && meta.RunnerOwnership == "" {
			if meta.RunnerExecutable != replacement.OldRunner {
				readErr = task.ErrIdentityMismatch
			} else {
				readErr = RecordManagedRunnerMigration(ctx, store.Root, td, meta)
			}
		}
		readErr = errors.Join(readErr, td.Close())
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func RecordManagedRunnerMigration(ctx context.Context, root string, td *taskdir.TaskDir, meta *task.MetaRecord) error {
	if td == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	if meta == nil || meta.RunnerOwnership != "" || meta.RunnerExecutable == "" {
		return nil
	}
	migrated, err := td.HasManagedRunnerMigrationContext(ctx)
	if err != nil || migrated {
		return err
	}
	if !managedRunnerExecutable(root, meta.RunnerExecutable, "") {
		return task.ErrIdentityMismatch
	}
	return td.RecordManagedRunnerMigrationContext(ctx)
}

func authorizeQueuedInspectionRunnerUpgrades(ctx context.Context, store *taskdir.Store, replacements []pueue.RunnerCommandReplacement) error {
	for _, replacement := range replacements {
		if !replacement.Inspection {
			continue
		}
		target, err := PlanQueuedRunnerRepairTarget(replacement.NewRunner)
		if err != nil {
			return err
		}
		if err := AuthorizeQueuedInspectionRunnerUpgrade(ctx, store, replacement, target); err != nil {
			return err
		}
	}
	return nil
}

func prepareQueuedRunnerCommandForJob(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskPrefix string) (pueue.RunnerCommandReplacement, bool, error) {
	if job.Label == nil {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	taskID, inspectionWorker, matched := queuedRunnerCommandIdentity(store.RootID, *job.Label, taskPrefix)
	if !matched {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	if inspectionWorker {
		return PrepareQueuedInspectionRunnerCommand(ctx, store, supervisor, job, taskID)
	}
	return prepareQueuedRunnerCommandContext(ctx, store, supervisor, job, taskID)
}

// ensurePrivateQueuedRunnerRepair performs the queue-wide migration at most
// once for one supervisor generation and target managed runner. New tasks use
// the target directly, while this transition repairs rows admitted by an
// earlier Delegate build. The receipt is published only after the exact Pueue
// replacement operation succeeds; an interruption retries safely.
func EnsurePrivateQueuedRunnerRepair(store *taskdir.Store, supervisor *pueue.Client, runnerExecutable string) error {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	target, err := PlanQueuedRunnerRepairTarget(runnerExecutable)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	return EnsurePrivateQueuedRunnerRepairContext(ctx, store, supervisor, target)
}

func EnsurePrivateQueuedRunnerRepairContext(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, target RunnerRepairTarget) error {
	if ctx == nil {
		return context.Canceled
	}
	receipt, name, err := newRunnerRepairReceipt(store, supervisor, target)
	if err != nil {
		return err
	}
	for {
		complete, receiptErr := runnerRepairReceiptCurrent(ctx, store, name, receipt)
		if receiptErr != nil {
			return receiptErr
		}
		if complete {
			return nil
		}
		acquired, transitionErr := pueue.TryWithPrivateTransition(ctx, store.Root, func() error {
			return repairPrivateQueuedRunnerTransition(ctx, store, supervisor, target, name, receipt)
		})
		if transitionErr != nil {
			return transitionErr
		}
		if acquired {
			return nil
		}
		if transitionErr = pueue.WaitForPrivateTransition(ctx, store.Root); transitionErr != nil {
			return transitionErr
		}
	}
}

func repairPrivateQueuedRunnerTransition(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, target RunnerRepairTarget, name string, receipt runnerRepairReceipt) error {
	// Another elected owner may have completed the same transition before
	// this caller acquired transition.lock. Recheck the immutable receipt
	// before reading or changing any queue row.
	complete, err := runnerRepairReceiptCurrent(ctx, store, name, receipt)
	if err != nil || complete {
		return err
	}
	if err = target.ValidateExecutable(target.executable); err != nil {
		return err
	}
	if err = repairQueuedRunnerCommandsToTarget(ctx, store, supervisor, target.executable); err != nil {
		return err
	}
	return publishRunnerRepairReceipt(ctx, store, name, receipt)
}

func newRunnerRepairReceipt(store *taskdir.Store, supervisor *pueue.Client, target RunnerRepairTarget) (runnerRepairReceipt, string, error) {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return runnerRepairReceipt{}, "", task.ErrInvalidPermit
	}
	if err := target.ValidateExecutable(target.executable); err != nil {
		return runnerRepairReceipt{}, "", err
	}
	receipt := runnerRepairReceipt{
		SchemaVersion:      task.SchemaVersion,
		Revision:           runnerRepairReceiptRevision,
		Supervisor:         supervisor.RuntimeBinding(),
		RunnerExecutable:   target.executable,
		ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
	}
	if err := validateRunnerRepairReceiptShape(store.Root, receipt); err != nil {
		return runnerRepairReceipt{}, "", err
	}
	data, err := task.MarshalCanonical(receipt)
	if err != nil {
		return runnerRepairReceipt{}, "", err
	}
	return receipt, "r-" + task.ComputeSHA256(data)[:48] + ".json", nil
}

func runnerRepairReceiptCurrent(ctx context.Context, store *taskdir.Store, name string, want runnerRepairReceipt) (current bool, resultErr error) {
	if store == nil {
		return false, task.ErrInvalidPermit
	}
	if err := validateRunnerRepairReceiptShape(store.Root, want); err != nil {
		return false, err
	}
	directory, err := store.OpenRunnerRepairContext(ctx, false)
	if task.RecordAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	var got runnerRepairReceipt
	if err = directory.Read(name, &got); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err = validateRunnerRepairReceiptShape(store.Root, got); err != nil {
		return false, err
	}
	if got != want {
		return false, task.ErrEvidenceFault
	}
	return true, nil
}

func publishRunnerRepairReceipt(ctx context.Context, store *taskdir.Store, name string, receipt runnerRepairReceipt) (resultErr error) {
	if store == nil {
		return task.ErrInvalidPermit
	}
	if err := validateRunnerRepairReceiptShape(store.Root, receipt); err != nil {
		return err
	}
	directory, err := store.OpenRunnerRepairContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	_, err = directory.PutContext(ctx, name, receipt)
	return err
}
