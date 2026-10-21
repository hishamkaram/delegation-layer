package app

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// runCheckpointAction executes one authenticated checkpoint helper inside the
// worker's supervised scope. Creation persists and verifies its immutable
// candidate before publishing exactly the stored payload; no commit or ref is
// written before that candidate. Removal deletes only the candidate's owned
// ref. The guardian-qualified receipt, not this result, is the disposition.
func runCheckpointAction(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, git workspace.Git) error {
	if err := workspacescore.ValidateCheckpointAction(ctx, store, operation); err != nil {
		return err
	}
	request := operation.Request()
	if request.Kind == workspace.OperationCheckpointRemove {
		return removeCheckpointRef(ctx, store, request, git)
	}
	scratch := workspace.ScratchPath(store.Root, request.OperationID)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return err
	}
	var prepared workspace.CheckpointCandidate
	var err error
	if request.Checkpoint.Kind == workspace.CheckpointManaged {
		prepared, err = prepareManagedCheckpoint(ctx, store, operation, git, scratch)
	} else {
		prepared, err = prepareOrdinaryCheckpoint(ctx, operation, git, scratch)
	}
	if err != nil {
		return err
	}
	if err = operation.RecordCheckpointCandidate(ctx, prepared); err != nil {
		return err
	}
	stored, err := operation.CheckpointCandidate(ctx)
	if err != nil {
		return err
	}
	if err = workspacescore.VerifyRepositoryIdentity(stored.Target.Source); err != nil {
		return err
	}
	return git.PublishCheckpoint(ctx, request, operation.Digest(), stored)
}

// prepareOrdinaryCheckpoint revalidates the selected directory's physical
// identity before resolving Git, then pins the actual common directory.
func prepareOrdinaryCheckpoint(ctx context.Context, operation *workspace.Operation, git workspace.Git, scratch string) (workspace.CheckpointCandidate, error) {
	selection := *operation.Request().Checkpoint
	if selection.CwdIdentity == nil {
		return workspace.CheckpointCandidate{}, task.ErrIdentityMismatch
	}
	if err := workspacescore.RejectUnmanagedPath(selection.Cwd); err != nil {
		return workspace.CheckpointCandidate{}, err
	}
	if err := workspacescore.VerifyDirectoryIdentity(selection.Cwd, *selection.CwdIdentity); err != nil {
		return workspace.CheckpointCandidate{}, err
	}
	target, err := git.ResolveOrdinaryCheckpointTarget(ctx, selection.Cwd)
	if err != nil {
		return workspace.CheckpointCandidate{}, err
	}
	if target.Source, err = pinCommonDirectory(target.Source); err != nil {
		return workspace.CheckpointCandidate{}, err
	}
	return git.PrepareOrdinaryCheckpoint(ctx, operation.Request(), operation.Digest(), target, scratch)
}

// prepareManagedCheckpoint observes the managed checkout only while this
// checkpoint holds its reservation and the registered topology still holds.
func prepareManagedCheckpoint(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, git workspace.Git, scratch string) (candidate workspace.CheckpointCandidate, resultErr error) {
	request := operation.Request()
	resource, err := workspace.LoadResource(ctx, store, request.WorkspaceID)
	if err != nil {
		return candidate, err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	reservation, err := resource.Reservation(ctx)
	if err != nil {
		return candidate, err
	}
	if reservation.Subject != (workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}) {
		return candidate, task.ErrIdentityMismatch
	}
	checkout, err := workspacescore.RegisteredCheckout(ctx, resource)
	if err != nil {
		return candidate, err
	}
	return git.PrepareManagedCheckpoint(ctx, request, operation.Digest(), checkout, scratch)
}

// removeCheckpointRef deletes the original candidate's owned ref in its pinned
// common directory; no source checkout is needed. The original is closed
// before any attempt is registered, so an absent candidate is final and there
// is nothing to remove.
func removeCheckpointRef(ctx context.Context, store *taskdir.Store, request workspace.OperationRequest, git workspace.Git) (resultErr error) {
	original, err := workspace.LoadOperation(ctx, store, request.Subject.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
	candidate, err := original.CheckpointCandidate(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = workspacescore.VerifyRepositoryIdentity(candidate.Target.Source); err != nil {
		return err
	}
	_, err = git.RemoveCheckpoint(ctx, request.RootID, candidate)
	return err
}
