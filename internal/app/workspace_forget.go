package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func forgetManagedWorkspace(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, git workspace.Git) error {
	intent, err := resource.ForgetIntent(ctx)
	if err != nil {
		return err
	}
	if _, err = resource.Removed(ctx); err != nil {
		return err
	}
	source, err := resource.Source(ctx)
	if workspace.OperationAbsent(err) && len(intent.Refs) == 0 {
		err = nil
	}
	if err != nil {
		return err
	}
	prefix := "refs/delegate/" + store.RootID + "/workspaces/" + resource.Request().WorkspaceID + "/"
	for _, reference := range intent.Refs {
		if !strings.HasPrefix(reference.Ref, prefix) {
			return task.ErrIdentityMismatch
		}
	}
	if err = git.DeleteRefs(ctx, source, prefix, intent.Refs); err != nil {
		return err
	}
	for _, artifact := range intent.Artifacts {
		if err = store.RemoveWorkspaceArtifact(ctx, artifact.OperationID, artifact.Name, artifact.Length, artifact.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func beginWorkspaceForget(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, budget time.Duration) (ack workspaceActionAcknowledgement, resultErr error) {
	if _, done, err := workspaceForgetAlreadyComplete(ctx, resource); done {
		ack.authorized = err == nil
		return ack, err
	}
	if err := requireWorkspaceForgetReady(ctx, resource); err != nil {
		return ack, err
	}
	if _, err := ensureWorkspaceForgetIntent(ctx, store, resource, budget); err != nil {
		return ack, err
	}
	ack.authorized = true
	ack.operationID, _, resultErr = reconcileWorkspaceForgetOperation(ctx, store, resource, deps, true, budget)
	return ack, resultErr
}

func workspaceForgetAlreadyComplete(ctx context.Context, resource *workspace.Resource) (operationID string, done bool, resultErr error) {
	forgotten, err := resource.Forgotten(ctx)
	if err == nil {
		return forgotten.OperationID, true, nil
	}
	if !workspace.OperationAbsent(err) {
		return "", true, err
	}
	return "", false, nil
}

func requireWorkspaceForgetReady(ctx context.Context, resource *workspace.Resource) error {
	if _, err := resource.Removed(ctx); err != nil {
		if workspace.OperationAbsent(err) {
			return workspace.ErrBusy
		}
		return err
	}
	if _, err := resource.Reservation(ctx); err == nil {
		return workspace.ErrBusy
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	return nil
}

func reconcileWorkspaceForgetOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, retry bool, budget time.Duration) (operationID string, handled bool, resultErr error) {
	intent, err := resource.ForgetIntent(ctx)
	if workspace.OperationAbsent(err) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	subject, err := workspaceForgetSubject(intent)
	if err != nil {
		return "", true, err
	}
	latest, err := latestWorkspaceOperation(ctx, store, resource, workspace.OperationForget, subject)
	if workspace.OperationAbsent(err) {
		var retryReady bool
		operationID, retryReady, resultErr = recoverFirstWorkspaceForget(ctx, store, resource, deps, intent, retry)
		if !retryReady {
			return operationID, true, resultErr
		}
		latest, err = latestWorkspaceOperation(ctx, store, resource, workspace.OperationForget, subject)
	}
	if err != nil {
		return "", true, err
	}
	operationID = latest.Request().OperationID
	result, _, completionErr := acceptedWorkspaceOperation(ctx, store, latest)
	if completionErr == nil && result.Status == "succeeded" {
		return operationID, true, completeWorkspaceForgetOperation(ctx, resource, latest)
	}
	if workspacePendingError(completionErr) {
		return operationID, true, errors.Join(completionErr, scheduleExistingWorkspaceOperation(ctx, store, latest, deps), latest.Close())
	}
	if !retry {
		return operationID, true, errors.Join(completionErr, latest.Close())
	}
	if _, err := workspaceOperationClosed(ctx, latest); err != nil {
		return "", true, errors.Join(err, latest.Close())
	}
	if err := latest.Close(); err != nil {
		return "", true, err
	}
	operationID, resultErr = submitWorkspaceForget(ctx, store, resource, deps, intent, budget, false)
	return operationID, true, resultErr
}

func recoverFirstWorkspaceForget(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, intent workspace.ForgetIntent, retry bool) (operationID string, retryReady bool, resultErr error) {
	operationID, resultErr = submitWorkspaceForget(ctx, store, resource, deps, intent, time.Duration(intent.BudgetNanos), true)
	if !retry || !workspacePendingError(resultErr) {
		return operationID, false, resultErr
	}
	deadline, err := time.Parse(time.RFC3339Nano, intent.Deadline)
	if err != nil {
		return operationID, false, errors.Join(resultErr, err)
	}
	return operationID, !time.Now().Before(deadline), resultErr
}

func completeWorkspaceForgetOperation(ctx context.Context, resource *workspace.Resource, operation *workspace.Operation) error {
	intent, err := resource.ForgetIntent(ctx)
	if err != nil {
		return errors.Join(err, operation.Close())
	}
	return errors.Join(resource.RecordForgotten(ctx, intent.OperationID), operation.Close())
}

func ensureWorkspaceForgetIntent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, budget time.Duration) (intent workspace.ForgetIntent, resultErr error) {
	intent, err := resource.ForgetIntent(ctx)
	if !workspace.OperationAbsent(err) {
		return intent, err
	}
	intent, err = workspaceForgetInventory(ctx, store, resource, budget)
	if err != nil {
		return intent, err
	}
	return intent, resource.RecordForgetIntent(ctx, intent)
}

func submitWorkspaceForget(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, intent workspace.ForgetIntent, budget time.Duration, first bool) (operationID string, resultErr error) {
	subject, err := workspaceForgetSubject(intent)
	if err != nil {
		return "", err
	}
	spec := workspaceOperationSpec{purpose: workspaceForgetPurpose(intent), kind: workspace.OperationForget, subject: subject, budget: budget, deadline: intent.Deadline}
	if !first {
		attempt, attemptErr := task.NewTaskID()
		if attemptErr != nil {
			return "", attemptErr
		}
		spec.purpose, spec.deadline = "forget-"+attempt+".json", ""
	}
	operation, err := scheduleWorkspaceOperation(ctx, store, resource, deps, spec, nil)
	defer func() { resultErr = errors.Join(resultErr, closeWorkspaceOperation(operation)) }()
	if operation != nil {
		operationID = operation.Request().OperationID
	}
	if err != nil {
		return operationID, err
	}
	return operationID, workspace.ErrPending
}

func workspaceForgetInventory(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, budget time.Duration) (intent workspace.ForgetIntent, resultErr error) {
	if budget <= 0 {
		return intent, task.ErrIdentityMismatch
	}
	id, err := task.NewTaskID()
	if err != nil {
		return intent, err
	}
	intent = workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: id,
		BudgetNanos: int64(budget), Deadline: time.Now().Add(budget).UTC().Format(time.RFC3339Nano),
	}
	source, sourceErr := resource.Source(ctx)
	if sourceErr == nil {
		intent.Refs = []workspace.ForgetRef{{Ref: managedBaseRef(store.RootID, resource.Request().WorkspaceID), ExpectedOID: source.BaseOID}}
	} else if !workspace.OperationAbsent(sourceErr) {
		return intent, sourceErr
	}
	if err = appendWorkspaceCaptureRefs(ctx, resource, &intent); err != nil {
		return intent, err
	}
	if err = appendWorkspaceArtifactInventory(ctx, store, resource, &intent); err != nil {
		return intent, err
	}
	sortWorkspaceForgetInventory(&intent)
	return intent, nil
}

func appendWorkspaceCaptureRefs(ctx context.Context, resource *workspace.Resource, intent *workspace.ForgetIntent) error {
	captureIDs, err := resource.CaptureIDs(ctx)
	if err != nil {
		return err
	}
	for _, captureID := range captureIDs {
		candidate, candidateErr := resource.CaptureCandidate(ctx, captureID)
		if candidateErr == nil {
			intent.Refs = append(intent.Refs, workspace.ForgetRef{Ref: candidate.SnapshotRef, ExpectedOID: candidate.TreeOID})
		} else if !workspace.OperationAbsent(candidateErr) {
			return candidateErr
		}
	}
	return nil
}

func appendWorkspaceArtifactInventory(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, intent *workspace.ForgetIntent) error {
	operationIDs, err := resource.OperationIDs(ctx)
	if err != nil {
		return err
	}
	for _, operationID := range operationIDs {
		operation, loadErr := workspace.LoadOperation(ctx, store, operationID)
		if loadErr != nil {
			return loadErr
		}
		result, completionErr := workspaceForgetOperationArtifacts(ctx, store, operation)
		closeErr := operation.Close()
		if closeErr != nil {
			return errors.Join(completionErr, closeErr)
		}
		if completionErr != nil {
			if errors.Is(completionErr, workspace.ErrPending) {
				return workspace.ErrBusy
			}
			if !workspace.OperationAbsent(completionErr) {
				return completionErr
			}
			continue
		}
		for _, artifact := range result.Artifacts {
			if artifact.Omitted == "" {
				intent.Artifacts = append(intent.Artifacts, workspace.ForgetArtifact{OperationID: operationID, Name: artifact.Name, Length: artifact.Length, SHA256: artifact.SHA256})
			}
		}
	}
	return nil
}

func workspaceForgetOperationArtifacts(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (workspace.OperationResult, error) {
	proof, err := workspaceOperationClosed(ctx, operation)
	if err != nil {
		return workspace.OperationResult{}, err
	}
	if proof.unlaunchedSHA256 != "" {
		return workspace.OperationResult{}, nil
	}
	result, err := operation.Result(ctx)
	if workspace.OperationAbsent(err) {
		return workspace.OperationResult{}, nil
	}
	if err != nil {
		return result, err
	}
	for _, artifact := range result.Artifacts {
		if err = validateWorkspaceArtifact(ctx, store, operation.Request().OperationID, artifact); err != nil {
			return result, err
		}
	}
	return result, nil
}

func sortWorkspaceForgetInventory(intent *workspace.ForgetIntent) {
	slices.SortFunc(intent.Refs, func(first, second workspace.ForgetRef) int { return strings.Compare(first.Ref, second.Ref) })
	slices.SortFunc(intent.Artifacts, func(first, second workspace.ForgetArtifact) int {
		return strings.Compare(first.OperationID+"/"+first.Name, second.OperationID+"/"+second.Name)
	})
}
