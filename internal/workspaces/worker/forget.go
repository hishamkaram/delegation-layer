package worker

import (
	"context"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
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
	} else if err == nil && len(intent.Refs) != 0 {
		err = workspacescore.VerifyRepositoryIdentity(source)
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
