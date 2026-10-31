package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func privateCleanupOwner() *supervisorcore.Cleanup {
	return supervisorcore.NewCleanup(inspectionCustodyEvidence{}, workspaces.InspectPrivateOperation)
}

// inspectionCustodyEvidence keeps the native inspection evidence policy and
// adds the custody release of a canonically closed inspection lifetime.
type inspectionCustodyEvidence struct{ admission.TerminalEvidence }

var _ supervisorcore.InspectionCustody = inspectionCustodyEvidence{}

func (inspectionCustodyEvidence) PrepareVerifiedLifetime(ctx context.Context, operation *inspection.Operation) error {
	return releaseLifetimeCustody(ctx, operation)
}

func (inspectionCustodyEvidence) AuthorizeInspectionRemoval(ctx context.Context, operation *inspection.Operation) (bool, error) {
	pending, err := lifetimeCustodyPending(ctx, operation)
	return err == nil && !pending, err
}
