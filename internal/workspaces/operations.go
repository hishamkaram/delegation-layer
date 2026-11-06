package workspaces

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
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
	// Verification and managed checkpoints are indexed only by the owner of
	// their accepted reservation. A failed Reserve must not leave an
	// unfinished authorized resource use.
	if request.Kind == workspace.OperationVerify || request.Kind == workspace.OperationCheckpoint {
		return nil
	}
	return resource.RecordOperation(ctx, request)
}

func createWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, spec OperationSpec) (operation *workspace.Operation, resultErr error) {
	request, err := newWorkspaceOperationRequest(ctx, store, deps, resource.Request().WorkspaceID, spec)
	if err != nil {
		return nil, err
	}
	operation, err = workspace.OpenOperation(ctx, store, request)
	if err != nil {
		return nil, err
	}
	winner, err := resource.AssociateOperation(ctx, spec.Purpose, request)
	if err != nil {
		return operation, err
	}
	if winner != request.OperationID {
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

// newWorkspaceOperationRequest binds one finite helper's deadline, published
// runtime binding, fresh identity and selected installed worker. Associating the
// request with a workspace purpose stays with its caller.
func newWorkspaceOperationRequest(ctx context.Context, store *taskdir.Store, deps OperationOptions, workspaceID string, spec OperationSpec) (workspace.OperationRequest, error) {
	deadline, err := workspaceOperationDeadline(spec)
	if err != nil {
		return workspace.OperationRequest{}, err
	}
	runner, supervisor, err := workspaceOperationRuntime(ctx, store, deps, deadline)
	if err != nil {
		return workspace.OperationRequest{}, err
	}
	if err = validateWorkspaceOperationExecutable(store.Root, runner); err != nil {
		return workspace.OperationRequest{}, err
	}
	id, err := randomid.NewTaskID()
	if err != nil {
		return workspace.OperationRequest{}, err
	}
	return workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: id,
		WorkspaceID: workspaceID, Kind: spec.Kind, Subject: spec.Subject,
		Deadline:         deadline,
		WorkerExecutable: runner, ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, Supervisor: supervisor,
		Arguments: spec.Arguments, Discard: spec.Discard, Checkpoint: cloneCheckpointSelection(spec.Checkpoint),
	}, nil
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
	if err := validateStoredWorkspaceActionRequest(ctx, store, operation); err != nil {
		return err
	}
	if awaiting, err := awaitingSubmission(ctx, operation); !awaiting {
		return err
	}
	source, err := supervisorcore.NewSavedBindingSource(request.RootID, infrastructure.SavedBindingWorkspaceRequest, request.OperationID, "", request.Supervisor)
	if err != nil {
		return err
	}
	supervisorOwner, err := supervisorcore.AcquireClient(deps.Runtime.OpenSaved(ctx, store, request.Supervisor, source))
	defer func() { resultErr = errors.Join(resultErr, supervisorOwner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return err
	}
	ctx, supervisor, err := workspaceSubmissionClient(ctx, supervisorOwner.Client(), request.Kind, deps)
	if err != nil {
		return err
	}
	identity := pueue.InspectionIdentity{RootID: request.RootID, TaskID: request.OperationID, QueueGroup: "default"}
	commit := func(observation pueue.InspectionObservation) error {
		if !time.Now().Before(operation.Deadline()) {
			return workspace.ErrPending
		}
		return LaunchPermissionError(operation.RecordReceipt(ctx, observation.Job.ID))
	}
	check := func(control context.Context) error {
		return workspaceOperationLaunchPermission(control, operation)
	}
	permit, err := operation.SubmissionPermit(ctx)
	if errors.Is(err, os.ErrExist) {
		return recoverWorkspaceSubmission(ctx, store.Root, operation, supervisor, identity, commit, check)
	}
	if err != nil {
		return LaunchPermissionError(err)
	}
	if err = validateWorkspaceOperationExecutable(store.Root, request.WorkerExecutable); err != nil {
		return err
	}
	launch := pueue.Launch{RunnerExecutable: request.WorkerExecutable, RootPath: store.Root, Supervised: true, WorkspaceOperation: true}
	checkedCommit := func(observation pueue.InspectionObservation) error {
		if commitErr := commit(observation); commitErr != nil {
			return commitErr
		}
		return check(ctx)
	}
	_, err = supervisor.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, supervisorcore.SubmissionGate(store.Root), checkedCommit)
	return err
}

// awaitingSubmission reports whether an operation still needs its supervisor
// submission: it is open, has no lifetime intent and is before its deadline.
// Otherwise it returns the operation's settled error, nil when nothing remains.
func awaitingSubmission(ctx context.Context, operation *workspace.Operation) (bool, error) {
	if _, err := OperationClosed(ctx, operation); err == nil {
		return false, nil
	} else if !errors.Is(err, workspace.ErrPending) || !PendingError(err) {
		return false, err
	}
	// The guardian publishes intent only after enqueue. A running helper does
	// not need another supervisor bind or queue reconciliation on every poll.
	if _, err := operation.ReadLifetimeIntentContext(ctx); err == nil {
		return false, nil
	} else if !workspace.OperationAbsent(err) {
		return false, err
	}
	if !time.Now().Before(operation.Deadline()) {
		return false, workspace.ErrPending
	}
	return true, nil
}

func recoverWorkspaceSubmission(ctx context.Context, root string, operation *workspace.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, commit pueue.InspectionObservationCommit, check func(context.Context) error) error {
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
		_, err = supervisorcore.RecoverStagedAuxiliaryObservationChecked(ctx, supervisor, root, observation, commit, matches, nil, check)
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
		var cleanup *taskdir.LifetimeOwnerCleanupError
		if errors.As(fenceErr, &cleanup) {
			return proof, fenceErr
		}
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
	if owner != nil && err != nil {
		return proof, err
	}
	if useOwnerContended(err) {
		return proof, workspace.ErrPending
	}
	if err != nil {
		return proof, err
	}
	defer func() { resultErr = owner.ReleaseWithError(resultErr) }()
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
	if err := validateStoredWorkspaceActionRequest(ctx, store, operation); err != nil {
		return workspace.OperationResult{}, guardian.ClosureProof{}, err
	}
	proof, err := OperationClosed(ctx, operation)
	if err != nil {
		return workspace.OperationResult{}, proof, err
	}
	return acceptedClosedWorkspaceOperation(ctx, store, operation, proof)
}

// The caller authenticates the stored action request and supplies canonical
// closure. Recovery and read-only observation share this result policy.
func acceptedClosedWorkspaceOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, proof guardian.ClosureProof) (workspace.OperationResult, guardian.ClosureProof, error) {
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
