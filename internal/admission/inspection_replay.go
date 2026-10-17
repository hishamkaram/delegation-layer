package admission

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func OpenAdmissionInspectionOperation(store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (operation *inspection.Operation, resultErr error) {
	return OpenInspectionOperationContext(context.Background(), store, request, binding, now)
}

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
	if err = rejectCompletedRuntimeIdentityDrift(ctx, operation, record.Binding, binding); err != nil {
		return err
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
		return task.ErrEvidenceFault
	}
	if record.Binding.WorkerExecutable == binding.WorkerExecutable && record.Binding.WorkerSHA256 == binding.WorkerSHA256 {
		return nil
	}
	return validateManagedInspectionReplay(ctx, store.Root, operation, binding)
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
// Completed markerless journals remain subject to the conservative identity
// drift rejection above because their proof cannot be relabeled in place.
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

// rejectCompletedRuntimeIdentityDrift prevents a completed capability proof
// from being relabeled as proof for a different provider executable. A queued
// or incomplete operation can still run its one worker against the current
// candidate; a completed operation has no safe way to rerun that worker under
// the same create-once journal.
func rejectCompletedRuntimeIdentityDrift(ctx context.Context, operation *inspection.Operation, saved, current inspection.Binding) error {
	if operation == nil ||
		(saved.CapabilitySHA256 == "" && current.CapabilitySHA256 == "") ||
		saved.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		current.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		saved.DefinitionSHA256 == current.DefinitionSHA256 {
		return nil
	}
	if _, _, err := operation.ReadCompletedContext(ctx); err == nil {
		return task.ErrEvidenceFault
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}

func validateManagedInspectionReplay(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	if !supervisorcore.ManagedInspectionWorkerExecutable(root, binding) {
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
