package app

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func reconcileWorkspaceUse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies) (resultError error) {
	return workspacescore.ReconcileUse(ctx, store, resource, workspaceRuntimeOptions(deps))
}

func workspaceAdmissionInspectionClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation) (resultErr error) {
	return workspacescore.AdmissionInspectionClosed(ctx, store, resource, reservation, admissioncore.RecoverInspectionLifetime)
}
