package admission

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// TerminalEvidence adapts the canonical inspection workflow decisions to the
// supervisor's evidence port. It has no state and cannot authorize queue removal.
type TerminalEvidence struct{}

func (TerminalEvidence) ValidateLifetime(ctx context.Context, operation *inspection.Operation, id string, job pueue.Job) error {
	return ValidateInspectionLifetime(ctx, operation, id, job)
}

func (TerminalEvidence) RecoverLifetime(ctx context.Context, operation *inspection.Operation) error {
	return RecoverInspectionLifetime(ctx, operation)
}

func (TerminalEvidence) RecordedTerminal(ctx context.Context, operation *inspection.Operation) (bool, error) {
	_, _, terminal, err := RecordedInspectionFacts(ctx, operation)
	if terminal && errors.Is(err, ErrInspectionWorkerUnavailable) {
		return true, nil
	}
	return terminal, err
}
