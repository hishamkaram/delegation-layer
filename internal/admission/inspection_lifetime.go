package admission

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func ValidateInspectionLifetime(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	if !job.Succeeded {
		historical, err := HistoricalInspectionLifetime(ctx, operation)
		if err != nil {
			return fmt.Errorf("%w: ended Delegate inspection %s lifetime termination is unproven: %w", inspection.ErrGroupMigrationRequired, taskID, err)
		}
		if historical {
			return nil
		}
		if _, err = guardian.ReadAuxiliaryClosureProof(ctx, operation); err != nil {
			return fmt.Errorf("%w: ended Delegate inspection %s lifetime termination is unproven: %w", inspection.ErrGroupMigrationRequired, taskID, err)
		}
		return nil
	}
	ready, err := SuccessfulInspectionLifetimeEvidence(ctx, operation)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s lifetime evidence is invalid: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	if !ready {
		return fmt.Errorf("%w: ended Delegate inspection %s is awaiting lifetime termination", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}
