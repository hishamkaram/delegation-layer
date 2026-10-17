package supervisor

import (
	"context"
	"errors"
	"log"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func RunTerminalEvent(root string, numericTaskID int64, exitCode string, options pueue.Options) error {
	eventContext, cancelEvent := context.WithTimeout(context.Background(), pueue.DefaultObservationTimeout)
	recorded, eventErr := pueue.RecordPrivateTerminalEventForMaintenance(eventContext, root, numericTaskID, exitCode)
	cancelEvent()
	if eventErr != nil {
		// Candidate classification is a cleanup hint, not task authority. Keep a
		// conservative full-scope wake durable even when its publication fails;
		// application journals still decide whether any row may be removed.
		log.Printf("delegate: terminal supervisor candidate deferred: %v", eventErr)
	}
	requestContext, cancelRequest := context.WithTimeout(context.Background(), pueue.DefaultObservationTimeout)
	defer cancelRequest()
	var requestErr error
	if eventErr == nil && recorded {
		requestErr = pueue.RequestPrivateCandidateQueueMaintenance(requestContext, root)
	} else {
		// Legacy rows without a callback ticket and failed candidate publication
		// take the conservative recovery scope. Current ordinary and inspection
		// submissions both publish exact event-backed candidates.
		requestErr = pueue.RequestPrivateQueueMaintenance(requestContext, root)
	}
	if requestErr != nil {
		if !recorded {
			return errors.Join(eventErr, requestErr)
		}
		// The terminal event itself is a durable cleanup inbox entry. Continue
		// with an elected pass even when the generation journal could not be
		// advanced; a later root activation can recover the retained event too.
		log.Printf("delegate: terminal supervisor cleanup request deferred: %v", requestErr)
	}
	// Pueue invokes this helper after a row reaches Done, so this process already
	// outlives the task creator. It owns the persisted wake to completion; task
	// and inspection journals remain the sole removal authority.
	if err := RunQueueMaintenance(root, options); err != nil {
		log.Printf("delegate: terminal supervisor cleanup deferred: %v", err)
	}
	return nil
}

func RunQueueMaintenance(root string, options pueue.Options) (resultErr error) {
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !found {
		return pueue.ErrBinding
	}
	supervisor, err := pueue.NewClient(binding, options)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, supervisor.Close())
	}()
	if !supervisor.RunRequestedPrivateQueueMaintenance(context.Background()) {
		return pueue.ErrConfiguration
	}
	return nil
}
