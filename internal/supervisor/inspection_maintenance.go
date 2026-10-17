package supervisor

import (
	"context"
	"log"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func RequestInspectionQueueMaintenance(ctx context.Context, supervisor *pueue.Client, observation pueue.InspectionObservation) bool {
	if ctx == nil || supervisor == nil {
		return false
	}
	eventContext, cancelEvent := context.WithTimeout(context.WithoutCancel(ctx), InspectionStatusWatchdog)
	eventFailed := false
	if observation.Matched && observation.Job.State == pueue.StateEnded {
		_, eventErr := supervisor.RecordPrivateTerminalObservation(eventContext, observation)
		if eventErr != nil {
			eventFailed = true
			log.Printf("delegation-layer: terminal inspection candidate deferred: %v", eventErr)
		}
	}
	cancelEvent()
	configPath := supervisor.Binding().ConfigPath
	stateRoot := filepath.Dir(filepath.Dir(configPath))
	if !pueue.IsPrivateConfig(stateRoot, configPath) {
		return false
	}
	requestContext, cancelRequest := context.WithTimeout(context.WithoutCancel(ctx), InspectionStatusWatchdog)
	defer cancelRequest()
	var requestErr error
	if eventFailed {
		requestErr = pueue.RequestPrivateQueueMaintenance(requestContext, stateRoot)
	} else {
		// Every independently accepted terminal observation owns a generation.
		// Suppressing this wake because another pass is currently pending can
		// strand the candidate when that concurrent pass settles as failed.
		requestErr = pueue.RequestPrivateCandidateQueueMaintenance(requestContext, stateRoot)
	}
	if requestErr != nil {
		log.Printf("delegation-layer: terminal inspection cleanup request deferred: %v", requestErr)
		if eventFailed {
			return false
		}
	}
	scheduled := supervisor.ScheduleRequestedPrivateQueueMaintenance(requestContext)
	if !scheduled {
		log.Printf("delegation-layer: terminal inspection cleanup owner deferred")
	}
	return scheduled
}
