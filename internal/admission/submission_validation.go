package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ValidateSubmissionRunner binds ordinary launches to immutable task metadata
// and, when present, the inspection request. A managed successor is accepted
// only after that exact request durably authorized its worker migration.
func ValidateSubmissionRunner(openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) (resultErr error) {
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
	operation, err := LoadInspectionOperationContext(context.Background(), store, *req)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return ValidateInspectionSubmissionRunner(operation, store, td, req, meta, runner)
}

func ValidateManagedRetrySubmissionRunner(openStore SubmissionStoreOpener, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
	if meta == nil {
		return task.ErrEvidenceFault
	}
	if err := supervisorcore.ValidateManagedRetryRunner(td, meta, runner); err != nil {
		return err
	}
	retryMeta := *meta
	// Validate the selected executable against inspection evidence while
	// retaining the task's recorded ownership classification.
	retryMeta.RunnerExecutable = runner
	return ValidateSubmissionRunner(openStore, td, req, &retryMeta, runner)
}

func ValidateInspectionSubmissionRunner(operation *inspection.Operation, store *taskdir.Store, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
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
		digest, fingerprintErr := commonprovider.FingerprintExecutable(runner)
		if fingerprintErr != nil {
			return task.ErrEvidenceFault
		}
		if digest != binding.WorkerSHA256 {
			return task.ErrEvidenceFault
		}
		return nil
	}
	return validateManagedInspectionSuccessor(operation, td, meta, runner, false)
}

func validateManagedInspectionSuccessor(operation *inspection.Operation, td *taskdir.TaskDir, meta *task.MetaRecord, runner string, sameWorker bool) error {
	binding := operation.Request().Binding
	if meta == nil || meta.RunnerOwnership != task.RunnerOwnershipManaged || binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return inspectionWorkerMismatch(sameWorker)
	}
	if err := supervisorcore.ValidateManagedRetryRunner(td, meta, runner); err != nil {
		return err
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(context.Background())
	if err != nil {
		return err
	}
	if !authorized {
		if _, proofErr := operation.ReadProofContext(context.Background()); proofErr != nil {
			if !errors.Is(proofErr, os.ErrNotExist) {
				return proofErr
			}
			return inspectionWorkerMismatch(sameWorker)
		}
		root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
		if !supervisorcore.IsStateRunnerPath(root, filepath.Clean(runner)) {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}

func inspectionWorkerMismatch(sameWorker bool) error {
	if sameWorker {
		return task.ErrEvidenceFault
	}
	return task.ErrIdentityMismatch
}
