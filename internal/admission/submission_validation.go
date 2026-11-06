package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ValidateSubmissionRunner binds ordinary launches to immutable task metadata
// and, when present, the inspection request. A managed successor is accepted
// only after that exact request durably authorized its worker migration.
func ValidateSubmissionRunner(openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) (resultErr error) {
	return ValidateSubmissionRunnerContext(context.Background(), openStore, td, req, meta, runner, configuredRole)
}

func ValidateSubmissionRunnerContext(ctx context.Context, openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) (resultErr error) {
	if td == nil || req == nil {
		return task.ErrEvidenceFault
	}
	if meta != nil && meta.RunnerExecutable != "" && runner != meta.RunnerExecutable {
		return task.ErrIdentityMismatch
	}
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	store, err := openStore(root)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	operation, err := LoadInspectionOperationContext(ctx, store, *req)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return ValidateInspectionSubmissionRunnerContext(ctx, operation, store, td, req, meta, runner, configuredRole)
}

func ValidateManagedRetrySubmissionRunner(openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) error {
	return ValidateManagedRetrySubmissionRunnerContext(context.Background(), openStore, td, req, meta, runner, configuredRole)
}

func ValidateManagedRetrySubmissionRunnerContext(ctx context.Context, openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) error {
	if meta == nil {
		return task.ErrEvidenceFault
	}
	target, err := managedSuccessorTarget(configuredRole, meta.RunnerExecutable, runner)
	if err != nil {
		return err
	}
	if err = supervisorcore.ValidateManagedRetryRunnerContext(ctx, td, meta, runner, target); err != nil {
		return err
	}
	retryMeta := *meta
	// Carry the proven managed ownership while checking the selected executable.
	// The original metadata and inspection binding remain immutable.
	retryMeta.RunnerOwnership = task.RunnerOwnershipManaged
	retryMeta.RunnerExecutable = runner
	return ValidateSubmissionRunnerContext(ctx, openStore, td, req, &retryMeta, runner, configuredRole)
}

func ValidateInspectionSubmissionRunner(operation *inspection.Operation, store *taskdir.Store, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) error {
	return ValidateInspectionSubmissionRunnerContext(context.Background(), operation, store, td, req, meta, runner, configuredRole)
}

func ValidateInspectionSubmissionRunnerContext(ctx context.Context, operation *inspection.Operation, store *taskdir.Store, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner, configuredRole string) error {
	request := operation.Request()
	requestBytes, err := task.MarshalCanonical(*req)
	if err != nil || request.RootID != store.RootID || request.TaskID != req.TaskID || request.TaskSHA256 != task.ComputeSHA256(requestBytes) {
		return task.ErrEvidenceFault
	}
	binding := request.Binding
	if meta != nil && !task.SameSupervisorIdentity(binding.Supervisor, meta.SupervisorConfig) {
		return task.ErrEvidenceFault
	}
	sameWorker := runner == binding.WorkerExecutable
	if sameWorker {
		return nil
	}

	return validateManagedInspectionSuccessor(ctx, operation, store.Root, td, meta, runner, configuredRole)
}

func validateManagedInspectionSuccessor(ctx context.Context, operation *inspection.Operation, root string, td *taskdir.TaskDir, meta *task.MetaRecord, runner, configuredRole string) error {
	binding := operation.Request().Binding
	if meta == nil || meta.RunnerOwnership != task.RunnerOwnershipManaged || !supervisorcore.ManagedInspectionBinding(root, binding) {
		return task.ErrIdentityMismatch
	}
	target, err := managedSuccessorTarget(configuredRole, binding.WorkerExecutable, runner)
	if err != nil {
		return err
	}
	if err = target.ValidateExecutable(runner); err != nil {
		return err
	}
	if err = supervisorcore.ValidateManagedRetryRunnerContext(ctx, td, meta, runner, target); err != nil {
		return err
	}
	authorized := false
	if binding.RunnerOwnership == task.RunnerOwnershipManaged {
		authorized, err = operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
		if err != nil {
			return err
		}
	}
	if !authorized {
		if _, proofErr := operation.ReadProofContext(ctx); proofErr != nil {
			if !errors.Is(proofErr, os.ErrNotExist) {
				return proofErr
			}
			return task.ErrIdentityMismatch
		}
	}
	return nil
}
