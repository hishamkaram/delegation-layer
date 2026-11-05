package supervisor

import (
	"context"
	"log"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func ResumeRetainedQueueMaintenance(stateRoot string, supervisor *pueue.Client) {
	ResumeRetainedQueueMaintenanceContext(context.Background(), stateRoot, supervisor)
}

func ResumeRetainedQueueMaintenanceContext(ctx context.Context, stateRoot string, supervisor *pueue.Client) {
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
	if err != nil {
		log.Printf("delegation-layer: retained terminal cleanup probe deferred: %v", err)
		return
	}
	if pending && !supervisor.ScheduleRequestedPrivateQueueMaintenance(ctx) {
		log.Printf("delegation-layer: retained terminal cleanup owner deferred")
	}
}
