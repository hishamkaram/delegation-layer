package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func privateCleanupOwner() *supervisorcore.Cleanup {
	return privateCleanupOwnerWithReporter(nil)
}

// inspectionCustodyEvidence keeps the native inspection evidence policy and
// adds the custody release of a canonically closed inspection lifetime.
type inspectionCustodyEvidence struct {
	admission.TerminalEvidence
	report func(error)
}

var _ supervisorcore.InspectionCustody = inspectionCustodyEvidence{}

func (e inspectionCustodyEvidence) PrepareVerifiedLifetime(ctx context.Context, operation *inspection.Operation) error {
	return releaseLifetimeCustody(ctx, operation, e.report)
}

func (inspectionCustodyEvidence) AuthorizeInspectionRemoval(ctx context.Context, operation *inspection.Operation) (bool, error) {
	pending, err := lifetimeCustodyPending(ctx, operation)
	return err == nil && !pending, err
}

func privateCleanupOwnerWithReporter(report func(error)) *supervisorcore.Cleanup {
	return supervisorcore.NewCleanup(inspectionCustodyEvidence{report: report}, workspaces.InspectPrivateOperation).WithDiagnosticReporter(report)
}
