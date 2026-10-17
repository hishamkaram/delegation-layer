package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// InspectionEvidence supplies the inspection owner's durable closure decision.
// Recovery remains a separate operation, used only during evidence preparation.
type InspectionEvidence interface {
	ValidateLifetime(context.Context, *inspection.Operation, string, pueue.Job) error
	RecoverLifetime(context.Context, *inspection.Operation) error
	RecordedTerminal(context.Context, *inspection.Operation) (bool, error)
}

// WorkspaceInspector dispatches an auxiliary label to its workspace journal.
// A false result leaves the label for the inspection journal owner.
type WorkspaceInspector func(context.Context, *taskdir.Store, inspection.GroupRequestRecord, string, pueue.Job) (bool, error)

// Cleanup owns queue-row eligibility. Its evidence ports are immutable; queue
// mutation and leases remain owned by the existing supervisor client.
type Cleanup struct {
	inspection InspectionEvidence
	workspace  WorkspaceInspector
}

func NewCleanup(inspectionEvidence InspectionEvidence, workspaceEvidence WorkspaceInspector) *Cleanup {
	return &Cleanup{inspection: inspectionEvidence, workspace: workspaceEvidence}
}
