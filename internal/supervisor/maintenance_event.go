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

func RunTerminalEvent(root string, numericTaskID int64, exitCode string, options pueue.Options, lookup ExecutableLookup) error {
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
	if err := RunQueueMaintenance(root, options, lookup); err != nil {
		log.Printf("delegate: terminal supervisor cleanup deferred: %v", err)
	}
	return nil
}

func RunQueueMaintenance(root string, options pueue.Options, lookup ExecutableLookup) (resultErr error) {
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !found {
		if binding, err = adoptLegacyPrivateSupervisor(root, options, lookup); err != nil {
			return err
		}
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

// adoptLegacyPrivateSupervisor publishes the logical binding of a live private
// daemon whose launch identity predates that record, so a maintenance wake on
// a historical root is not refused forever. It is a bounded recovery: the
// canonical installation resolver supplies the pair, and
// EnsurePrivateSupervisorBindingWithLock under the bootstrap EX lease remains
// the sole authority. It never creates a config, starts or restarts a queue,
// or relaxes a daemon, path, hash or generation check; a pair that cannot
// prove the recorded owner is refused and the durable wake stays pending. It
// enters only an existing bootstrap boundary, so a late callback cannot
// recreate a root that disposal removed or that never published a supervisor.
func adoptLegacyPrivateSupervisor(root string, options pueue.Options, lookup ExecutableLookup) (task.SupervisorRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(options))
	defer cancel()
	err := pueue.WithExistingBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		if _, found, err := pueue.ReadPrivateSupervisorBinding(root); err != nil || found {
			return err
		}
		daemonRecorded, err := pueue.PrivateDaemonIdentityPresent(root)
		if err != nil {
			return err
		}
		if !daemonRecorded {
			return fmt.Errorf("%w: private supervisor has no recorded daemon identity", pueue.ErrBinding)
		}
		if _, err = os.Lstat(pueue.PrivateConfigPath(root)); err != nil {
			return fmt.Errorf("%w: legacy private supervisor config: %w", pueue.ErrBinding, err)
		}
		clientExecutable, daemonExecutable, err := ResolvePrivateSupervisorExecutables(root, lookup)
		if err != nil {
			return err
		}
		supervisor, err := pueue.PreparePrivateWithLock(lockedContext, clientExecutable, daemonExecutable, root, options)
		if err != nil {
			return err
		}
		err = supervisor.EnsurePrivateSupervisorBindingWithLock(lockedContext, root)
		return errors.Join(err, supervisor.CloseAfterPending(err))
	})
	if err != nil {
		return task.SupervisorRef{}, err
	}
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err == nil && !found {
		err = pueue.ErrBinding
	}
	return binding, err
}
