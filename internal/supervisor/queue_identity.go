package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func PrivateMigrationSupervisorMatches(stateRoot string, current, candidate task.SupervisorRef) (bool, error) {
	if task.SameSupervisorIdentity(current, candidate) {
		return true, nil
	}
	return pueue.PrivateSupervisorGenerationMatches(stateRoot, candidate)
}

func ValidateInspectionRunner(operation *inspection.Operation, runner string) error {
	if operation == nil || runner == "" {
		return task.ErrEvidenceFault
	}
	binding := operation.Request().Binding
	if runner != binding.WorkerExecutable {
		return task.ErrIdentityMismatch
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil || digest != binding.WorkerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func prepareQueuedRunnerCommand(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string) (_ pueue.RunnerCommandReplacement, eligible bool, resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	td, err := store.OpenTask(taskID)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	return queuedRunnerCommandReplacement(store, supervisor, job, taskID, td)
}

func PrepareQueuedInspectionRunnerCommand(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string) (_ pueue.RunnerCommandReplacement, eligible bool, resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	if owned, err := queuedWorkspaceOperation(ctx, store, supervisor, job, taskID); owned || err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := request.SupervisorIdentity()
	if err = validateQueuedInspectionIdentity(store, supervisor, job, request, identity); err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	eligible, err = queuedInspectionWorkerUpgradeEligible(ctx, store.Root, operation, request.Binding)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	if !eligible {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	return pueue.RunnerCommandReplacement{
		NumericID:       job.ID,
		RootID:          store.RootID,
		TaskID:          taskID,
		Label:           identity.Label(),
		RootPath:        store.Root,
		OldRunner:       request.Binding.WorkerExecutable,
		Inspection:      true,
		InspectionGroup: request.Group,
	}, true, nil
}

func validateQueuedInspectionIdentity(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, request inspection.RequestRecord, identity pueue.InspectionIdentity) error {
	if request.RootID != store.RootID || request.IdentityID() != identity.TaskID || request.Task.RootID != store.RootID || request.Task.TaskID != request.TaskID || request.Group != identity.Group() || request.Label != identity.Label() {
		return task.ErrIdentityMismatch
	}
	if job.Label == nil || *job.Label != identity.Label() || job.Group != identity.Group() || !task.SameSupervisorIdentity(request.Binding.Supervisor, supervisor.Binding()) {
		return pueue.ErrBinding
	}
	return nil
}

func queuedInspectionWorkerUpgradeEligible(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) (bool, error) {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return false, nil
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil || authorized {
		return authorized, err
	}
	if !ManagedInspectionWorkerExecutable(root, binding) {
		return false, nil
	}
	if err = ValidateInspectionRunner(operation, binding.WorkerExecutable); err != nil {
		return false, err
	}
	return true, nil
}

func AuthorizeQueuedInspectionRunnerUpgrade(ctx context.Context, store *taskdir.Store, replacement pueue.RunnerCommandReplacement) (resultErr error) {
	if store == nil || !replacement.Inspection || replacement.RootID != store.RootID || replacement.RootPath != store.Root {
		return task.ErrIdentityMismatch
	}
	if !CurrentManagedRunnerExecutable(store.Root, replacement.NewRunner) {
		return task.ErrEvidenceFault
	}
	operation, err := inspection.LoadOperationContext(ctx, store, replacement.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := request.SupervisorIdentity()
	if err = validateInspectionReplacementIdentity(store, replacement, request, identity); err != nil {
		return err
	}
	return authorizeInspectionManagedWorker(ctx, store.Root, operation, request.Binding)
}

func authorizeInspectionManagedWorker(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil {
		return err
	}
	if authorized {
		return nil
	}
	if !ManagedInspectionWorkerExecutable(root, binding) {
		return task.ErrEvidenceFault
	}
	if err = ValidateInspectionRunner(operation, binding.WorkerExecutable); err != nil {
		return err
	}
	return operation.AuthorizeManagedWorkerUpgradeContext(ctx)
}

func queuedRunnerCommandReplacement(store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, taskID string, td *taskdir.TaskDir) (pueue.RunnerCommandReplacement, bool, error) {
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	label := "delegate:" + store.RootID + ":" + taskID
	if req.RootID != store.RootID || req.TaskID != taskID || submission.RootID != store.RootID || submission.TaskID != taskID || submission.Label != label {
		return pueue.RunnerCommandReplacement{}, false, task.ErrIdentityMismatch
	}
	if job.Label == nil || *job.Label != label || !task.SameSupervisorIdentity(submission.Supervisor, meta.SupervisorConfig) || !task.SameSupervisorIdentity(supervisor.Binding(), submission.Supervisor) {
		return pueue.RunnerCommandReplacement{}, false, pueue.ErrBinding
	}
	if meta.RunnerExecutable == "" {
		return pueue.RunnerCommandReplacement{}, false, task.ErrEvidenceFault
	}
	ownership, err := RunnerOwnershipForTask(store.Root, td, meta)
	if err != nil {
		return pueue.RunnerCommandReplacement{}, false, err
	}
	if ownership != task.RunnerOwnershipManaged {
		return pueue.RunnerCommandReplacement{}, false, nil
	}
	return pueue.RunnerCommandReplacement{
		NumericID: job.ID,
		RootID:    store.RootID,
		TaskID:    taskID,
		Label:     label,
		RootPath:  store.Root,
		OldRunner: meta.RunnerExecutable,
	}, true, nil
}

// queuedWorkspaceOperation preserves a helper's immutable content-addressed
// runner. Package upgrades do not remove that state-owned executable. This queue
// transition cannot replace or relabel it as a provider inspection.
func queuedWorkspaceOperation(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, id string) (owned bool, resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: id, QueueGroup: "default"}
	matches, err := PrivateMigrationSupervisorMatches(store.Root, supervisor.Binding(), request.Supervisor)
	if err != nil {
		return true, err
	}
	if !matches || job.Label == nil || *job.Label != identity.Label() || job.Group != identity.Group() {
		return true, task.ErrIdentityMismatch
	}
	return true, nil
}
