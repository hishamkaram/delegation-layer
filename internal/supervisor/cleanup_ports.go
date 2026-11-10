package supervisor

import (
	"context"
	"errors"

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

// InspectionCustody is an optional InspectionEvidence extension for the
// custody of an inspection's verified executables. Both methods run only after
// the row's lifetime evidence was validated, inside the operation's existing
// Close ownership. PrepareVerifiedLifetime releases a canonically closed
// lifetime's custody; ReleaseInspectionCustody calls it while no generation
// lease or queue identity is held. AuthorizeInspectionRemoval reports whether
// that custody is released; false or an error retains only this row, so a
// later pass retries the exact recorded pool.
type InspectionCustody interface {
	PrepareVerifiedLifetime(context.Context, *inspection.Operation) error
	AuthorizeInspectionRemoval(context.Context, *inspection.Operation) (bool, error)
}

// WorkspaceInspector dispatches an auxiliary label to its workspace journal.
// A false result leaves the label for the inspection journal owner.
type WorkspaceInspector func(context.Context, *taskdir.Store, inspection.GroupRequestRecord, string, pueue.Job) (bool, error)

// Cleanup owns queue-row eligibility. Its evidence ports are immutable; queue
// mutation and leases remain owned by the existing supervisor client.
type Cleanup struct {
	report     func(error)
	inspection InspectionEvidence
	workspace  WorkspaceInspector
	// custody is the evidence's optional InspectionCustody, resolved once.
	custody InspectionCustody
}

func NewCleanup(inspectionEvidence InspectionEvidence, workspaceEvidence WorkspaceInspector) *Cleanup {
	cleanup := &Cleanup{inspection: inspectionEvidence, workspace: workspaceEvidence}
	if custody, ok := inspectionEvidence.(InspectionCustody); ok {
		cleanup.custody = custody
	}
	return cleanup
}

// errInspectionCustodyPending retains a row whose verified executables may
// still be held in custody.
var errInspectionCustodyPending = errors.New("inspection custody release is pending")

// removalGate is the custody check applied to an ended inspection row after
// its terminal evidence was validated. Nil means no custody owner is installed.
func (c *Cleanup) removalGate() func(context.Context, *inspection.Operation) error {
	if c.custody == nil {
		return nil
	}
	return func(ctx context.Context, operation *inspection.Operation) error {
		released, err := c.custody.AuthorizeInspectionRemoval(ctx, operation)
		if err != nil {
			return err
		}
		if !released {
			return errInspectionCustodyPending
		}
		return nil
	}
}

// WithDiagnosticReporter binds immutable invocation telemetry without changing
// queue eligibility, leases or the evidence owner's removal policy.
func (c *Cleanup) WithDiagnosticReporter(report func(error)) *Cleanup {
	copy := *c
	copy.report = report
	return &copy
}
