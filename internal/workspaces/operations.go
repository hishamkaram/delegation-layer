package workspaces

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// ensureWorkspaceOperation authorizes a finite helper only for a durable
// purpose. Recovering that purpose retains its first deadline and add guard.
func ensureWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, spec OperationSpec) (operation *workspace.Operation, resultErr error) {
	reference, err := resource.AssociatedOperation(ctx, spec.Purpose)
	if err == nil {
		return loadWorkspacePurpose(ctx, store, resource, reference, spec)
	}
	if !workspace.OperationAbsent(err) {
		return nil, err
	}
	return createWorkspaceOperation(ctx, store, resource, deps, spec)
}

func loadWorkspacePurpose(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reference workspace.OperationReference, spec OperationSpec) (*workspace.Operation, error) {
	operation, err := readWorkspacePurpose(ctx, store, resource, reference, spec)
	if err != nil {
		return nil, err
	}
	if err = bindWorkspacePurpose(ctx, resource, operation.Request(), spec); err != nil {
		return nil, errors.Join(err, operation.Close())
	}
	return operation, nil
}

func readWorkspacePurpose(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reference workspace.OperationReference, spec OperationSpec) (*workspace.Operation, error) {
	if reference.Kind != spec.Kind {
		return nil, task.ErrIdentityMismatch
	}
	operation, err := workspace.LoadOperation(ctx, store, reference.OperationID)
	if err != nil {
		return nil, err
	}
	if err = errors.Join(validateWorkspaceOperationSpec(operation.Request(), spec), ValidateActionRequest(ctx, resource, operation.Request())); err != nil {
		return nil, errors.Join(err, operation.Close())
	}
	return operation, nil
}

func bindWorkspacePurpose(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest, spec OperationSpec) error {
	if err := validateWorkspaceOperationSpec(request, spec); err != nil {
		return err
	}
	if err := ValidateActionRequest(ctx, resource, request); err != nil {
		return err
	}
	// Verification is indexed only by the owner of its accepted reservation.
	// A failed Reserve must not leave an unfinished authorized resource use.
	if request.Kind == workspace.OperationVerify {
		return nil
	}
	return resource.RecordOperation(ctx, request)
}

func createWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, spec OperationSpec) (operation *workspace.Operation, resultErr error) {
	deadline, err := workspaceOperationDeadline(spec)
	if err != nil {
		return nil, err
	}
	runner, supervisor, err := workspaceOperationRuntime(ctx, store, deps, deadline)
	if err != nil {
		return nil, err
	}
	id, err := randomid.NewTaskID()
	if err != nil {
		return nil, err
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		return nil, err
	}
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: id,
		WorkspaceID: resource.Request().WorkspaceID, Kind: spec.Kind, Subject: spec.Subject,
		Deadline:         deadline,
		WorkerExecutable: runner, WorkerSHA256: digest, Supervisor: supervisor,
		Arguments: spec.Arguments, Discard: spec.Discard,
	}
	operation, err = workspace.OpenOperation(ctx, store, request)
	if err != nil {
		return nil, err
	}
	winner, err := resource.AssociateOperation(ctx, spec.Purpose, request)
	if err != nil {
		return operation, err
	}
	if winner != id {
		if err = operation.Close(); err != nil {
			return nil, err
		}
		operation, err = workspace.LoadOperation(ctx, store, winner)
		if err != nil {
			return nil, err
		}
	}
	if err = bindWorkspacePurpose(ctx, resource, operation.Request(), spec); err != nil {
		return operation, err
	}
	return operation, nil
}

func workspaceOperationRuntime(ctx context.Context, store *taskdir.Store, deps OperationOptions, deadline string) (runner string, binding task.SupervisorRef, resultErr error) {
	runner, err := deps.Runtime.ManagedRunner()
	if err != nil {
		return "", binding, err
	}
	cutoff, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		return "", binding, err
	}
	if !time.Now().Before(cutoff) {
		var found bool
		binding, found, err = pueue.ReadPrivateSupervisorBinding(store.Root)
		if err != nil || !found {
			return "", binding, errors.Join(task.ErrEvidenceFault, err)
		}
		return runner, binding, ctx.Err()
	}
	runner, client, err := deps.Runtime.PrepareManaged(ctx, store, runner)
	owner, err := supervisorcore.AcquireClient(client, err)
	defer func() { resultErr = errors.Join(resultErr, owner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return "", binding, err
	}
	return runner, owner.Client().Binding(), nil
}

func workspaceOperationDeadline(spec OperationSpec) (string, error) {
	if spec.Budget <= 0 {
		return "", task.ErrIdentityMismatch
	}
	if spec.Deadline == "" {
		return time.Now().Add(spec.Budget).UTC().Format(time.RFC3339Nano), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, spec.Deadline)
	if err != nil || parsed.IsZero() || parsed.UTC().Format(time.RFC3339Nano) != spec.Deadline {
		return "", task.ErrIdentityMismatch
	}
	return spec.Deadline, nil
}

func SubmitOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, deps OperationOptions) (resultErr error) {
	defer func() {
		resultErr = ControlError(resultErr)
	}()
	request := operation.Request()
	if err := validateStoredWorkspaceActionRequest(ctx, store, request); err != nil {
		return err
	}
	if _, err := OperationClosed(ctx, operation); err == nil {
		return nil
	} else if !errors.Is(err, workspace.ErrPending) {
		return err
	}
	// The guardian publishes intent only after enqueue. A running helper does
	// not need another supervisor bind or queue reconciliation on every poll.
	if _, err := operation.ReadLifetimeIntentContext(ctx); err == nil {
		return nil
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	if !time.Now().Before(operation.Deadline()) {
		return workspace.ErrPending
	}
	supervisorOwner, err := supervisorcore.AcquireClient(deps.Runtime.OpenSaved(ctx, store.Root, request.Supervisor))
	defer func() { resultErr = errors.Join(resultErr, supervisorOwner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return err
	}
	supervisor := supervisorOwner.Client()
	if request.Kind == workspace.OperationVerify {
		// Only the submitted verification job receives the caller environment.
		// Binding and cold daemon startup retain bounded control options.
		supervisor, err = supervisor.WithCommandEnvironment(os.Environ())
		if err != nil {
			return err
		}
	}
	identity := pueue.InspectionIdentity{RootID: request.RootID, TaskID: request.OperationID, QueueGroup: "default"}
	commit := func(observation pueue.InspectionObservation) error {
		if !time.Now().Before(operation.Deadline()) {
			return workspace.ErrPending
		}
		return LaunchPermissionError(operation.RecordReceipt(ctx, observation.Job.ID))
	}
	permit, err := operation.SubmissionPermit(ctx)
	if errors.Is(err, os.ErrExist) {
		return recoverWorkspaceSubmission(ctx, store.Root, operation, supervisor, identity, commit)
	}
	if err != nil {
		return LaunchPermissionError(err)
	}
	digest, err := commonprovider.FingerprintExecutable(request.WorkerExecutable)
	if err != nil || digest != request.WorkerSHA256 {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	launch := pueue.Launch{RunnerExecutable: request.WorkerExecutable, RootPath: store.Root, Supervised: true, WorkspaceOperation: true}
	_, err = supervisor.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, supervisorcore.SubmissionGate(store.Root), commit)
	return err
}

func recoverWorkspaceSubmission(ctx context.Context, root string, operation *workspace.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, commit pueue.InspectionObservationCommit) error {
	receipt, err := operation.Receipt(ctx)
	if err == nil {
		identity.NumericTaskID = &receipt.NumericTaskID
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	observation, err := supervisor.ReconcileInspection(ctx, identity)
	if err != nil {
		return err
	}
	if !observation.Matched {
		return workspace.ErrPending
	}
	if observation.Job.State == pueue.StateStashed {
		matches := func(id int64) error { return workspaceSchedulingReceiptMatches(ctx, operation, id) }
		_, err = supervisorcore.RecoverStagedAuxiliaryObservation(ctx, supervisor, root, observation, commit, matches, nil)
		return err
	}
	return workspaceSchedulingReceiptMatches(ctx, operation, observation.Job.ID)
}

func workspaceSchedulingReceiptMatches(ctx context.Context, operation *workspace.Operation, id int64) error {
	receipt, err := operation.Receipt(ctx)
	if err != nil {
		return err
	}
	if receipt.NumericTaskID != id {
		return task.ErrIdentityMismatch
	}
	return nil
}

// workspaceOperationClosed accepts the shared lifetime reader's canonical
// proofs. Conversation IDs and provider publication never grant reuse.
func OperationClosed(ctx context.Context, operation *workspace.Operation) (guardian.ClosureProof, error) {
	proof, err := readOperationProof(ctx, operation)
	if !errors.Is(err, workspace.ErrPending) {
		return proof, err
	}
	if !time.Now().Before(operation.Deadline()) {
		_, fenceErr := operation.SealLifetimeUnlaunchedContext(ctx)
		if fenceErr == nil {
			return readOperationProof(ctx, operation)
		}
		if !workspace.OperationAbsent(fenceErr) && !errors.Is(fenceErr, task.ErrLockBusy) && !errors.Is(fenceErr, task.ErrAlreadyStarted) && !errors.Is(fenceErr, task.ErrTerminalTask) && !errors.Is(fenceErr, task.ErrLifetimeDeadlineNotExpired) {
			return proof, fenceErr
		}
	}
	return recoverWorkspaceNativeClosure(ctx, operation)
}

func recoverWorkspaceNativeClosure(ctx context.Context, operation *workspace.Operation) (proof guardian.ClosureProof, resultErr error) {
	owner, err := operation.TryAcquireLifetimeOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return proof, workspace.ErrPending
	}
	if err != nil {
		return proof, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	if _, err = operation.ReadLifetimeIntentContext(ctx); workspace.OperationAbsent(err) {
		return proof, workspace.ErrPending
	} else if err != nil {
		return proof, err
	}
	if err = guardian.ReconcileRecordsOwned(ctx, operation); err != nil {
		return proof, err
	}
	return workspaceNativeOperationProof(ctx, operation)
}

func workspaceNativeOperationProof(ctx context.Context, operation *workspace.Operation) (guardian.ClosureProof, error) {
	return ReadLifetimeProof(ctx, operation)
}

func AcceptedOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (workspace.OperationResult, guardian.ClosureProof, error) {
	if err := validateStoredWorkspaceActionRequest(ctx, store, operation.Request()); err != nil {
		return workspace.OperationResult{}, guardian.ClosureProof{}, err
	}
	proof, err := OperationClosed(ctx, operation)
	if err != nil {
		return workspace.OperationResult{}, proof, err
	}
	if proof.UnlaunchedSHA256() != "" || proof.Kind() == task.LifetimeClosureUnregisteredRecovered {
		return workspace.OperationResult{}, proof, task.ErrLifetimeUnlaunched
	}
	result, err := operation.Result(ctx)
	if workspace.OperationAbsent(err) {
		return result, proof, ErrOperationResultUnavailable
	}
	if err != nil {
		return result, proof, err
	}
	for _, artifact := range result.Artifacts {
		if err = ValidateArtifact(ctx, store, operation.Request().OperationID, artifact); err != nil {
			return result, proof, err
		}
	}
	if result.Status != "succeeded" {
		return result, proof, &OperationError{reason: result.ReasonCode}
	}
	return result, proof, nil
}
