package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// checkpointIntent is the canonical caller intent of one checkpoint creation:
// the workspace (empty for an ordinary checkout), a fresh intent ID and the
// complete validated selection. Its digest is the creation request's admission
// subject; the request's own immutable fields carry it, so it needs no journal.
type checkpointIntent struct {
	WorkspaceID string                        `json:"workspace_id"`
	ID          string                        `json:"id"`
	Checkpoint  workspace.CheckpointSelection `json:"checkpoint"`
}

// CheckpointPurpose names the managed workspace purpose of one creation intent.
func CheckpointPurpose(intentID string) string {
	return "checkpoint-" + intentID + ".json"
}

// checkpointIntentSubject is the one constructor of a creation's subject.
func checkpointIntentSubject(workspaceID, intentID string, selection workspace.CheckpointSelection) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(checkpointIntent{WorkspaceID: workspaceID, ID: intentID, Checkpoint: selection})
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectAdmission, ID: intentID, SHA256: task.ComputeSHA256(data)}, nil
}

// validateCheckpointCreationIntent recomputes the creation subject from the
// request's own workspace and selection; a request whose subject was not
// derived from them is refused before scheduling, execution or acceptance.
func validateCheckpointCreationIntent(request workspace.OperationRequest) error {
	if request.Kind != workspace.OperationCheckpoint || request.Checkpoint == nil || request.Subject.Kind != workspace.SubjectAdmission {
		return task.ErrIdentityMismatch
	}
	subject, err := checkpointIntentSubject(request.WorkspaceID, request.Subject.ID, *request.Checkpoint)
	if err != nil {
		return err
	}
	if subject != request.Subject {
		return task.ErrIdentityMismatch
	}
	return nil
}

// checkpointRemovalSubject names the original checkpoint and its exact
// creation request digest; every removal attempt of it carries this subject.
func checkpointRemovalSubject(original *workspace.Operation) workspace.Subject {
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: original.Request().OperationID, SHA256: original.Digest()}
}

// loadCheckpointCreation loads an authenticated checkpoint creation.
func loadCheckpointCreation(ctx context.Context, store *taskdir.Store, checkpointID string) (*workspace.Operation, error) {
	operation, err := workspace.LoadOperation(ctx, store, checkpointID)
	if err != nil {
		return nil, err
	}
	if err = validateCheckpointCreationIntent(operation.Request()); err != nil {
		return nil, errors.Join(err, operation.Close())
	}
	return operation, nil
}

// validateRegisteredCheckpointRemoval accepts a removal only as an attempt
// registered in its original checkpoint journal. An unregistered request,
// such as a lost registration race, is refused even if invoked directly.
func validateRegisteredCheckpointRemoval(ctx context.Context, store *taskdir.Store, removal *workspace.Operation) (resultErr error) {
	original, err := loadCheckpointCreation(ctx, store, removal.Request().Subject.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
	_, err = registeredCheckpointRemoval(ctx, original, removal)
	return err
}

// registeredCheckpointRemoval returns removal's authenticated attempt in the
// original journal: same checkpoint subject, operation and request digest.
func registeredCheckpointRemoval(ctx context.Context, original, removal *workspace.Operation) (workspace.CheckpointRemovalReference, error) {
	if removal.Request().Kind != workspace.OperationCheckpointRemove || removal.Request().Subject != checkpointRemovalSubject(original) {
		return workspace.CheckpointRemovalReference{}, task.ErrIdentityMismatch
	}
	attempts, err := original.CheckpointRemovalAttempts(ctx)
	if err != nil {
		return workspace.CheckpointRemovalReference{}, err
	}
	for _, attempt := range attempts {
		if attempt.OperationID != removal.Request().OperationID {
			continue
		}
		if attempt.RequestSHA256 != removal.Digest() {
			return workspace.CheckpointRemovalReference{}, task.ErrIdentityMismatch
		}
		return attempt, nil
	}
	return workspace.CheckpointRemovalReference{}, task.ErrIdentityMismatch
}

// ValidateCheckpointAction applies the canonical stored-action policy to a
// checkpoint creation or removal before its worker performs any effect.
func ValidateCheckpointAction(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) error {
	if !checkpointOperationKind(operation.Request().Kind) {
		return task.ErrIdentityMismatch
	}
	return validateStoredWorkspaceActionRequest(ctx, store, operation)
}

func checkpointOperationKind(kind string) bool {
	return kind == workspace.OperationCheckpoint || kind == workspace.OperationCheckpointRemove
}

// OrdinaryCheckpointSelection pins an ordinary checkout directory the caller
// has already canonicalized, including actual directory-entry spelling. A
// path inside a managed workspace is refused: that workspace is selected only
// by its ID, so no path alias bypasses its reservation.
func OrdinaryCheckpointSelection(canonicalCwd string) (workspace.CheckpointSelection, error) {
	if err := RejectUnmanagedPath(canonicalCwd); err != nil {
		return workspace.CheckpointSelection{}, err
	}
	identity, err := DirectoryIdentity(canonicalCwd)
	if err != nil {
		return workspace.CheckpointSelection{}, err
	}
	return workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: canonicalCwd, CwdIdentity: &identity}, nil
}

func equalCheckpointSelection(first, second *workspace.CheckpointSelection) bool {
	if first == nil || second == nil {
		return first == second
	}
	if (first.CwdIdentity == nil) != (second.CwdIdentity == nil) || (first.CwdIdentity != nil && *first.CwdIdentity != *second.CwdIdentity) {
		return false
	}
	return first.Kind == second.Kind && first.Cwd == second.Cwd
}

func cloneCheckpointSelection(selection *workspace.CheckpointSelection) *workspace.CheckpointSelection {
	if selection == nil {
		return nil
	}
	clone := *selection
	if selection.CwdIdentity != nil {
		identity := *selection.CwdIdentity
		clone.CwdIdentity = &identity
	}
	return &clone
}
