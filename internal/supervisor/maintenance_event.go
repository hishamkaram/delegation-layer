package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func RunTerminalEventWithRuntime(root string, numericTaskID int64, exitCode string, runtime AdmissionOptions) error {
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
	if err := RunQueueMaintenanceWithRuntime(root, runtime); err != nil {
		log.Printf("delegate: terminal supervisor cleanup deferred: %v", err)
	}
	return nil
}

func RunQueueMaintenanceWithRuntime(root string, runtime AdmissionOptions) (resultErr error) {
	options := runtime.Control
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !found {
		if binding, err = prepareLegacyPrivateMaintenance(root, runtime); err != nil {
			return err
		}
	}
	selectedClient, err := ResolveSavedSupervisorClient(root, binding, runtime.Lookup())
	if err != nil {
		return err
	}
	options.RuntimeClientExecutable = selectedClient
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

// Legacy maintenance must retain an existing bootstrap/daemon/config boundary.
// The local prefix observes eligibility only; independent preparation owns every
// generation publication and start after that existing boundary is released.
func prepareLegacyPrivateMaintenance(root string, runtime AdmissionOptions) (binding task.SupervisorRef, resultErr error) {
	ctx, cancel := ControlContext(runtime.Control)
	defer cancel()
	err := pueue.WithExistingBootstrapLockContext(ctx, root, func(locked context.Context) error {
		recorded, err := pueue.PrivateDaemonIdentityPresent(root)
		if err != nil {
			return err
		}
		if !recorded {
			return fmt.Errorf("%w: private supervisor has no recorded daemon identity", pueue.ErrBinding)
		}
		if _, err = os.Lstat(pueue.PrivateConfigPath(root)); err != nil {
			return fmt.Errorf("%w: legacy private supervisor config: %w", pueue.ErrBinding, err)
		}
		return locked.Err()
	})
	if err != nil {
		return binding, err
	}
	if runtime.OpenStore == nil {
		return binding, ErrInfrastructureAuthorityUnavailable
	}
	store, err := runtime.OpenStore(ctx, root)
	if err != nil {
		return binding, err
	}
	defer func() {
		if !CleanupRetainsStore(resultErr, store) {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	client, err := (ControlRuntime{Options: runtime.Control, Lookup: runtime.Lookup(), Routing: runtime.Routing}).PreparePrivateAdmission(ctx, store)
	if err = ClosePrivateAdmissionTransition(client, err); err != nil {
		return binding, err
	}
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err == nil && !found {
		err = pueue.ErrBinding
	}
	return binding, err
}
