package admission

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func OpenInspectionOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (operation *inspection.Operation, resultErr error) {
	operation, err := LoadInspectionOperationContext(ctx, store, request)
	if task.RecordAbsent(err) {
		return CreateAdmissionInspectionOperationContext(ctx, store, request, binding, now)
	}
	if err != nil {
		return nil, err
	}
	keepOperation := false
	defer func() {
		if !keepOperation {
			resultErr = errors.Join(resultErr, operation.Close())
		}
	}()

	if err = validateAdmissionInspectionReplay(ctx, store, request, binding, operation); err != nil {
		return nil, err
	}
	keepOperation = true
	return operation, nil
}

func validateAdmissionInspectionReplay(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, operation *inspection.Operation) error {
	record := operation.Request()
	requestBytes, err := task.MarshalCanonical(request)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if record.RootID != store.RootID || record.TaskID != request.TaskID || record.TaskSHA256 != task.ComputeSHA256(requestBytes) {
		return task.ErrEvidenceFault
	}
	identity, err := OperationID(ctx, store, request)
	if err != nil || record.IdentityID() != identity {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	allowMarkerlessReplay, err := allowIncompleteMarkerlessRuntimeReplay(ctx, operation, record.Binding, binding)
	if err != nil || allowMarkerlessReplay {
		// A permitted markerless replay keeps its original create-once request.
		// The worker establishes the refreshed capability contract in new facts.
		return err
	}
	if sameInspectionBinding(record.Binding, binding) {
		return nil
	}
	if !sameInspectionBindingExceptManagedWorker(record.Binding, binding) {
		return validateCompletedLegacyInspectionReplay(ctx, store.Root, operation, record.Binding, binding)
	}
	if record.Binding.WorkerExecutable == binding.WorkerExecutable && record.Binding.WorkerSHA256 == binding.WorkerSHA256 {
		return nil
	}
	return validateManagedInspectionReplay(ctx, operation, binding)
}

func validateCompletedLegacyInspectionReplay(ctx context.Context, root string, operation *inspection.Operation, saved, current inspection.Binding) error {
	if saved.RunnerOwnership != "" || saved.WorkerExitProof != "" ||
		!supervisorcore.ManagedInspectionBinding(root, saved) ||
		!supervisorcore.ManagedInspectionWorkerExecutable(current) ||
		current.WorkerExitProof != inspection.WorkerExitProofGuardianWaitV1 ||
		!sameInspectionBindingBase(saved, current) {
		return task.ErrEvidenceFault
	}
	_, err := operation.ReadProofContext(ctx)
	if task.RecordAbsent(err) {
		return task.ErrEvidenceFault
	}
	return err
}

func CreateAdmissionInspectionOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (*inspection.Operation, error) {
	identity, err := OperationID(ctx, store, request)
	if err != nil {
		return nil, err
	}
	if ConcurrentInspection(store.Root, binding) {
		if request.Workspace != nil {
			return inspection.OpenConcurrentOperationWithIdentityContext(ctx, store, request, binding, identity, now)
		}
		return inspection.OpenConcurrentOperationContext(ctx, store, request, binding, now)
	}
	if request.Workspace != nil {
		return inspection.OpenOperationWithIdentityContext(ctx, store, request, binding, identity, now)
	}
	return inspection.OpenOperationContext(ctx, store, request, binding, now)
}

// allowIncompleteMarkerlessRuntimeReplay lets a pre-capability-marker runtime
// journal survive a provider or probe-contract refresh while it is still
// incomplete. Its immutable worker, supervisor, and environment bindings must
// remain unchanged; only the runtime identity/definition may be re-probed.
// A completed markerless journal has no capability contract to compare, so
// its changed definition remains refused by the exact binding comparison.
func allowIncompleteMarkerlessRuntimeReplay(ctx context.Context, operation *inspection.Operation, saved, current inspection.Binding) (bool, error) {
	if operation == nil || !markerlessRuntimeReplayBindingMatches(saved, current) {
		return false, nil
	}
	savedEnvironmentRecorded := saved.EnvironmentRecorded || len(saved.Environment) > 0
	currentEnvironmentRecorded := current.EnvironmentRecorded || len(current.Environment) > 0
	if savedEnvironmentRecorded != currentEnvironmentRecorded || !slices.Equal(saved.Environment, current.Environment) {
		return false, nil
	}
	_, _, err := operation.ReadCompletedContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func validateManagedInspectionReplay(ctx context.Context, operation *inspection.Operation, binding inspection.Binding) error {
	if !supervisorcore.ManagedInspectionWorkerExecutable(binding) {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil {
		return err
	}
	if authorized {
		return nil
	}
	// A retry may reopen a completed operation after its queued worker is gone.
	// The later supervisor reconciliation still verifies the matching
	// successful job before these facts can admit the request.
	result, _, err := operation.ReadCompletedContext(ctx)
	if err != nil || result.Reason != inspection.ResultEligible {
		return task.ErrEvidenceFault
	}
	return nil
}
