package app

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type workspaceOperationSpec struct {
	purpose   string
	kind      string
	subject   workspace.Subject
	budget    time.Duration
	deadline  string
	arguments []string
	discard   bool
}

// ensureWorkspaceOperation authorizes a finite helper only for a durable
// purpose. Recovering that purpose retains its first deadline and add guard.
func ensureWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, spec workspaceOperationSpec) (operation *workspace.Operation, resultErr error) {
	reference, err := resource.AssociatedOperation(ctx, spec.purpose)
	if err == nil {
		return loadWorkspacePurpose(ctx, store, resource, reference, spec)
	}
	if !workspace.OperationAbsent(err) {
		return nil, err
	}
	return createWorkspaceOperation(ctx, store, resource, deps, spec)
}

func loadWorkspacePurpose(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reference workspace.OperationReference, spec workspaceOperationSpec) (*workspace.Operation, error) {
	operation, err := readWorkspacePurpose(ctx, store, resource, reference, spec)
	if err != nil {
		return nil, err
	}
	if err = bindWorkspacePurpose(ctx, resource, operation.Request(), spec); err != nil {
		return nil, errors.Join(err, operation.Close())
	}
	return operation, nil
}

func readWorkspacePurpose(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reference workspace.OperationReference, spec workspaceOperationSpec) (*workspace.Operation, error) {
	if reference.Kind != spec.kind {
		return nil, task.ErrIdentityMismatch
	}
	operation, err := workspace.LoadOperation(ctx, store, reference.OperationID)
	if err != nil {
		return nil, err
	}
	if err = errors.Join(validateWorkspaceOperationSpec(operation.Request(), spec), validateWorkspaceActionRequest(ctx, resource, operation.Request())); err != nil {
		return nil, errors.Join(err, operation.Close())
	}
	return operation, nil
}

func bindWorkspacePurpose(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest, spec workspaceOperationSpec) error {
	if err := validateWorkspaceOperationSpec(request, spec); err != nil {
		return err
	}
	if err := validateWorkspaceActionRequest(ctx, resource, request); err != nil {
		return err
	}
	// Verification is indexed only by the owner of its accepted reservation.
	// A failed Reserve must not leave an unfinished authorized resource use.
	if request.Kind == workspace.OperationVerify {
		return nil
	}
	return resource.RecordOperation(ctx, request)
}

func validateWorkspaceOperationSpec(request workspace.OperationRequest, spec workspaceOperationSpec) error {
	if request.Kind != spec.kind || request.Subject != spec.subject || request.Discard != spec.discard || !slices.Equal(request.Arguments, spec.arguments) {
		return task.ErrIdentityMismatch
	}
	if spec.deadline != "" && request.Deadline != spec.deadline {
		return task.ErrIdentityMismatch
	}
	return nil
}

func createWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, spec workspaceOperationSpec) (operation *workspace.Operation, resultErr error) {
	deadline, err := workspaceOperationDeadline(spec)
	if err != nil {
		return nil, err
	}
	runner, supervisor, err := workspaceOperationRuntime(ctx, store, deps, deadline)
	if err != nil {
		return nil, err
	}
	id, err := task.NewTaskID()
	if err != nil {
		return nil, err
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		return nil, err
	}
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: id,
		WorkspaceID: resource.Request().WorkspaceID, Kind: spec.kind, Subject: spec.subject,
		Deadline:         deadline,
		WorkerExecutable: runner, WorkerSHA256: digest, Supervisor: supervisor,
		Arguments: spec.arguments, Discard: spec.discard,
	}
	operation, err = workspace.OpenOperation(ctx, store, request)
	if err != nil {
		return nil, err
	}
	winner, err := resource.AssociateOperation(ctx, spec.purpose, request)
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

func workspaceOperationRuntime(ctx context.Context, store *taskdir.Store, deps Dependencies, deadline string) (runner string, binding task.SupervisorRef, resultErr error) {
	runner, err := resolveAdmissionRunner("", task.RunnerOwnershipManaged, deps)
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
	runner, err = preparePrivateAdmissionExecutablesContext(ctx, Arguments{Root: store.Root}, deps, store.Root, runner, task.RunnerOwnershipManaged)
	if err != nil {
		return "", binding, err
	}
	options := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	owner, err := acquireSupervisorClient(preparePrivateAdmissionSupervisorContext(ctx, deps, store, options))
	defer func() { resultErr = errors.Join(resultErr, owner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return "", binding, err
	}
	return runner, owner.Client().Binding(), nil
}

func workspaceOperationDeadline(spec workspaceOperationSpec) (string, error) {
	if spec.budget <= 0 {
		return "", task.ErrIdentityMismatch
	}
	if spec.deadline == "" {
		return time.Now().Add(spec.budget).UTC().Format(time.RFC3339Nano), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, spec.deadline)
	if err != nil || parsed.IsZero() || parsed.UTC().Format(time.RFC3339Nano) != spec.deadline {
		return "", task.ErrIdentityMismatch
	}
	return spec.deadline, nil
}

func submitWorkspaceOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, deps Dependencies) (resultErr error) {
	defer func() {
		resultErr = workspaceControlError(resultErr)
	}()
	request := operation.Request()
	if err := validateStoredWorkspaceActionRequest(ctx, store, request); err != nil {
		return err
	}
	if _, err := workspaceOperationClosed(ctx, operation); err == nil {
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
	options := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	supervisorOwner, err := acquireSupervisorClient(newSupervisorClient(ctx, store.Root, request.Supervisor, options, true, deps.InitialSupervisorExecutable))
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
		return workspaceLaunchPermissionError(operation.RecordReceipt(ctx, observation.Job.ID))
	}
	permit, err := operation.SubmissionPermit(ctx)
	if errors.Is(err, os.ErrExist) {
		return recoverWorkspaceSubmission(ctx, store.Root, operation, supervisor, identity, commit)
	}
	if err != nil {
		return workspaceLaunchPermissionError(err)
	}
	digest, err := commonprovider.FingerprintExecutable(request.WorkerExecutable)
	if err != nil || digest != request.WorkerSHA256 {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	launch := pueue.Launch{RunnerExecutable: request.WorkerExecutable, RootPath: store.Root, Supervised: true, WorkspaceOperation: true}
	_, err = supervisor.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, privateSubmissionGate(store.Root), commit)
	return err
}

// A guardian can close its native scope between observation and the scheduling
// gate. Its launch refusal is pending until the next canonical closure read;
// it must neither fail a completed helper nor authorize another enqueue.
func workspaceLaunchPermissionError(err error) error {
	if errors.Is(err, task.ErrTerminalTask) || errors.Is(err, task.ErrLifetimeUnlaunched) {
		return workspace.ErrPending
	}
	return err
}

func workspaceControlError(err error) error {
	if errors.Is(err, pueue.ErrInFlight) || errors.Is(err, pueue.ErrSubmissionUncertain) || errors.Is(err, pueue.ErrEnqueueUncertain) {
		return errors.Join(workspace.ErrPending, err)
	}
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
		_, err = recoverStagedAuxiliaryObservation(ctx, supervisor, root, observation, commit, matches, nil)
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

type workspaceOperationProof struct {
	closureKind       task.LifetimeClosureKind
	terminationSHA256 string
	workerWaitSHA256  string
	unlaunchedSHA256  string
}

// Closure classification comes from the canonical lifetime resolver. Hashes
// bind the records that exist for that disposition; requiring every native
// hash here would reject valid recovery dispositions.
func (p workspaceOperationProof) closed() bool {
	return (task.LifetimeClosure{Kind: p.closureKind}).Closed() || p.unlaunchedSHA256 != ""
}

// workspaceOperationClosed accepts the shared lifetime reader's canonical
// proofs. Conversation IDs and provider publication never grant reuse.
func workspaceOperationClosed(ctx context.Context, operation *workspace.Operation) (workspaceOperationProof, error) {
	proof, err := readWorkspaceOperationProof(ctx, operation)
	if !errors.Is(err, workspace.ErrPending) {
		return proof, err
	}
	if !time.Now().Before(operation.Deadline()) {
		_, fenceErr := operation.SealLifetimeUnlaunchedContext(ctx)
		if fenceErr == nil {
			return readWorkspaceOperationProof(ctx, operation)
		}
		if !workspace.OperationAbsent(fenceErr) && !errors.Is(fenceErr, task.ErrLockBusy) && !errors.Is(fenceErr, task.ErrAlreadyStarted) && !errors.Is(fenceErr, task.ErrTerminalTask) && !errors.Is(fenceErr, task.ErrLifetimeDeadlineNotExpired) {
			return proof, fenceErr
		}
	}
	return recoverWorkspaceNativeClosure(ctx, operation)
}

// Authorization reads existing proof only. Recovery and fencing remain owned
// by the workspace coordinator, outside supervisor generation/queue leases.
func readWorkspaceOperationProof(ctx context.Context, operation *workspace.Operation) (workspaceOperationProof, error) {
	return readAuxiliaryLifetimeProof(ctx, operation)
}

type auxiliaryLifetimeEvidence interface {
	workspaceLifetimeEvidence
	ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error)
}

func readAuxiliaryLifetimeProof(ctx context.Context, operation auxiliaryLifetimeEvidence) (workspaceOperationProof, error) {
	proof, err := readWorkspaceLifetimeProof(ctx, operation)
	if !errors.Is(err, workspace.ErrPending) {
		return proof, err
	}
	fence, err := operation.ReadLifetimeUnlaunchedContext(ctx)
	if workspace.OperationAbsent(err) {
		return proof, workspace.ErrPending
	}
	if err != nil {
		return proof, err
	}
	data, err := task.MarshalCanonical(fence)
	if err != nil {
		return proof, err
	}
	return workspaceOperationProof{unlaunchedSHA256: task.ComputeSHA256(data)}, nil
}

func recoverWorkspaceNativeClosure(ctx context.Context, operation *workspace.Operation) (proof workspaceOperationProof, resultErr error) {
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
	if err = reconcileControlLifetimeOwned(ctx, operation); err != nil {
		return proof, err
	}
	return workspaceNativeOperationProof(ctx, operation)
}

func workspaceNativeOperationProof(ctx context.Context, operation *workspace.Operation) (workspaceOperationProof, error) {
	return readWorkspaceLifetimeProof(ctx, operation)
}

// workspaceLifetimeEvidence is implemented by task and both auxiliary record
// families. Their native closure and exact Wait requirements have one reader.
type workspaceLifetimeEvidence interface {
	taskdir.LifetimeRecordReader
}

func readWorkspaceLifetimeProof(ctx context.Context, records workspaceLifetimeEvidence) (workspaceOperationProof, error) {
	evidence, err := taskdir.LoadLifetimeEvidenceContext(ctx, records)
	if err != nil {
		return workspaceOperationProof{}, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return workspaceOperationProof{}, err
	}
	if !closure.Closed() {
		return workspaceOperationProof{}, workspace.ErrPending
	}
	proof := workspaceOperationProof{closureKind: closure.Kind}
	if evidence.Termination != nil {
		data, marshalErr := task.MarshalCanonical(evidence.Termination)
		if marshalErr != nil {
			return proof, marshalErr
		}
		proof.terminationSHA256 = task.ComputeSHA256(data)
	}
	if evidence.WorkerWait != nil {
		data, marshalErr := task.MarshalCanonical(evidence.WorkerWait)
		if marshalErr != nil {
			return proof, marshalErr
		}
		proof.workerWaitSHA256 = task.ComputeSHA256(data)
	}
	return proof, nil
}

var errWorkspaceOperationResultUnavailable = errors.New("workspace operation ended without a result")

func acceptedWorkspaceOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (workspace.OperationResult, workspaceOperationProof, error) {
	if err := validateStoredWorkspaceActionRequest(ctx, store, operation.Request()); err != nil {
		return workspace.OperationResult{}, workspaceOperationProof{}, err
	}
	proof, err := workspaceOperationClosed(ctx, operation)
	if err != nil {
		return workspace.OperationResult{}, proof, err
	}
	if proof.unlaunchedSHA256 != "" || proof.closureKind == task.LifetimeClosureUnregisteredRecovered {
		return workspace.OperationResult{}, proof, task.ErrLifetimeUnlaunched
	}
	result, err := operation.Result(ctx)
	if workspace.OperationAbsent(err) {
		return result, proof, errWorkspaceOperationResultUnavailable
	}
	if err != nil {
		return result, proof, err
	}
	for _, artifact := range result.Artifacts {
		if err = validateWorkspaceArtifact(ctx, store, operation.Request().OperationID, artifact); err != nil {
			return result, proof, err
		}
	}
	if result.Status != "succeeded" {
		return result, proof, &workspaceOperationError{reason: result.ReasonCode}
	}
	return result, proof, nil
}

type workspaceOperationError struct{ reason string }

func (e *workspaceOperationError) Error() string {
	if e.reason == "" {
		return "workspace operation failed"
	}
	return "workspace operation failed: " + e.reason
}
