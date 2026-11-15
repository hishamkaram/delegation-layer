package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// FailureReason selects the existing closed recovery reason; empty reason data
// must not hide the underlying pending or observation failure.
func FailureReason(err error) string {
	// Closure-only maintenance and joined faults cannot supply queue activity.
	// Legacy errors may lack an assessment; preserve their pure busy reason.
	queueBusy := MaintenanceBusy(err)
	var maintenance *MaintenanceError
	if errors.As(err, &maintenance) && maintenance.Assessment.ActiveWork != "present" {
		queueBusy = false
	}
	var recovery *pueue.PrivateRecoveryError
	if errors.As(err, &recovery) && recovery.ReasonCode != "" && infrastructure.PreparationReason(recovery.ReasonCode).Validate() == nil {
		if recovery.ReasonCode != "active_work" || queueBusy {
			return recovery.ReasonCode
		}
	}
	switch {
	case queueBusy && (errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) || errors.Is(err, inspection.ErrGroupMigrationBusy)):
		return "active_work"
	case errors.Is(err, pueue.ErrInFlight), errors.Is(err, pueue.ErrStatusOperational), errors.Is(err, pueue.ErrControlLimit),
		errors.Is(err, pueue.ErrUnknown), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "observation_failed"
	default:
		return ""
	}
}
