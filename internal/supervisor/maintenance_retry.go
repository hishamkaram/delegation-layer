package supervisor

import (
	"context"
	"log"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func ResumeRetainedQueueMaintenance(stateRoot string, supervisor *pueue.Client) {
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeeded(stateRoot)
	if err != nil {
		log.Printf("delegation-layer: retained terminal cleanup probe deferred: %v", err)
		return
	}
	if pending && !supervisor.ScheduleRequestedPrivateQueueMaintenance(context.Background()) {
		log.Printf("delegation-layer: retained terminal cleanup owner deferred")
	}
}
