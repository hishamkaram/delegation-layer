package supervisor

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func ResumeRetainedQueueMaintenanceContext(ctx context.Context, stateRoot string, supervisor *pueue.Client) {
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
	if err != nil {
		supervisor.ReportDiagnostic(fmt.Errorf("delegation-layer: retained terminal cleanup probe deferred: %w", err))
		return
	}
	if pending && !supervisor.ScheduleRequestedPrivateQueueMaintenance(ctx) {
		supervisor.ReportDiagnostic(fmt.Errorf("delegation-layer: retained terminal cleanup owner deferred"))
	}
}
