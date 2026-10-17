package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func ValidateArtifact(ctx context.Context, store *taskdir.Store, operationID string, artifact workspace.Artifact) error {
	if err := workspace.ValidateArtifact(artifact); err != nil {
		return err
	}
	if artifact.Omitted != "" {
		return validateWorkspaceOmittedArtifact(artifact)
	}
	err := store.VerifyWorkspaceArtifact(ctx, operationID, artifact.Name, artifact.Length, artifact.SHA256)
	if !isWorkspaceArtifactMissing(err) {
		return err
	}
	return validateIntentionallyForgottenArtifact(ctx, store, operationID, artifact, err)
}

func validateIntentionallyForgottenArtifact(ctx context.Context, store *taskdir.Store, operationID string, artifact workspace.Artifact, missingErr error) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return errors.Join(missingErr, err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	resource, err := workspace.LoadResource(ctx, store, operation.Request().WorkspaceID)
	if err != nil {
		return errors.Join(missingErr, err)
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	intent, err := resource.ForgetIntent(ctx)
	if workspace.OperationAbsent(err) {
		return missingErr
	}
	if err != nil {
		return errors.Join(missingErr, err)
	}
	for _, forgotten := range intent.Artifacts {
		if forgotten.OperationID == operationID && forgotten.Name == artifact.Name && forgotten.Length == artifact.Length && forgotten.SHA256 == artifact.SHA256 {
			return nil
		}
	}
	return missingErr
}
