package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// EnsureAdmissionScheduling routes resumed tasks through the same
// private-supervisor scheduling policy as a fresh admission. Recovery may
// reuse an already-running generation, so it cannot rely on the fresh-start
// transition to have published the receipt. The shared probe is no-change
// only; one elected transition owner rechecks and repairs under the bootstrap
// lock when the receipt or observed group state is stale.
func EnsureAdmissionScheduling(store *taskdir.Store, supervisor *pueue.Client, options pueue.Options) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private admission supervisor")
	}
	controlContext, cancel := ControlContext(options)
	defer cancel()
	return EnsureAdmissionSchedulingContext(controlContext, store.Root, supervisor)
}

// EnsureAdmissionSchedulingContext reuses the receipt or elects its repair owner.
func EnsureAdmissionSchedulingContext(controlContext context.Context, stateRoot string, supervisor *pueue.Client) error {
	if controlContext == nil {
		return context.Canceled
	}
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return errors.New("missing private admission supervisor")
	}
	for {
		recorded, err := OrdinarySchedulingRecorded(stateRoot, supervisor.RuntimeBinding())
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
		acquired, transitionErr := pueue.TryWithPrivateTransition(controlContext, stateRoot, func() error {
			return pueue.WithBootstrapLockContext(controlContext, stateRoot, func(lockedContext context.Context) error {
				return EnsureOrdinarySchedulingWithBootstrapLock(lockedContext, stateRoot, supervisor)
			})
		})
		if transitionErr != nil {
			return transitionErr
		}
		if acquired {
			return nil
		}
		if transitionErr = pueue.WaitForPrivateTransition(controlContext, stateRoot); transitionErr != nil {
			return transitionErr
		}
	}
}

// OrdinarySchedulingRecorded validates the receipt for this exact generation.
func OrdinarySchedulingRecorded(stateRoot string, binding task.SupervisorRef) (bool, error) {
	_, recorded, err := privateOrdinarySchedulingAuthorization(stateRoot, binding)
	return recorded, err
}

func privateOrdinarySchedulingAuthorization(stateRoot string, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return pueue.SubmissionAuthorization{}, false, pueue.ErrConfiguration
	}
	data, found, err := pueue.ReadPrivateSchedulingEvidence(stateRoot)
	if err != nil || !found {
		return pueue.SubmissionAuthorization{}, false, err
	}
	authorization, recorded, err := decodePrivateOrdinaryScheduling(data, binding)
	if err != nil {
		return pueue.SubmissionAuthorization{}, false, errors.Join(pueue.ErrBinding, err)
	}
	return authorization, recorded, nil
}

// SubmissionGate is the shared authorization for task and auxiliary submissions.
func SubmissionGate(stateRoot string) pueue.SubmissionGate {
	return func(_ context.Context, binding task.SupervisorRef) (pueue.SubmissionAuthorization, error) {
		if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
			return pueue.SubmissionAuthorization{}, nil
		}
		complete, err := pueue.PrivateSupervisorMigrationComplete(stateRoot, binding)
		if err != nil {
			return pueue.SubmissionAuthorization{}, err
		}
		if !complete {
			return pueue.SubmissionAuthorization{}, pueue.ErrPrivateSupervisorSubmissionIncomplete
		}
		authorization, recorded, err := privateOrdinarySchedulingAuthorization(stateRoot, binding)
		if err != nil {
			return pueue.SubmissionAuthorization{}, err
		}
		if !recorded {
			return pueue.SubmissionAuthorization{}, pueue.ErrBinding
		}
		return authorization, nil
	}
}

// OrdinarySchedulingConfiguredGroups checks the receipt and observed default group.
func OrdinarySchedulingConfiguredGroups(stateRoot string, binding task.SupervisorRef, groups map[string]pueue.Group) (bool, error) {
	recorded, err := OrdinarySchedulingRecorded(stateRoot, binding)
	if err != nil || !recorded {
		return recorded, err
	}
	group, found := groups["default"]
	return found && group.ParallelTasks == 0, nil
}

// EnsureOrdinarySchedulingWithBootstrapLock preserves or repairs the receipt under the caller's bootstrap lock.
func EnsureOrdinarySchedulingWithBootstrapLock(ctx context.Context, stateRoot string, supervisor *pueue.Client) error {
	recorded, err := OrdinarySchedulingRecorded(stateRoot, supervisor.RuntimeBinding())
	if err != nil || recorded {
		return err
	}
	return RepairOrdinarySchedulingWithBootstrapLock(ctx, stateRoot, supervisor)
}

// RepairOrdinarySchedulingWithBootstrapLock publishes observed scheduling and staged capability under the caller's bootstrap lock.
func RepairOrdinarySchedulingWithBootstrapLock(ctx context.Context, stateRoot string, supervisor *pueue.Client) error {
	groups, err := supervisor.SetPrivateDefaultParallelTasksWithBootstrapLock(ctx, stateRoot, 0)
	if err != nil {
		return err
	}
	group, found := groups["default"]
	if !found || group.ParallelTasks != 0 {
		return fmt.Errorf("%w: private supervisor did not retain unlimited ordinary task parallelism", pueue.ErrBinding)
	}
	capability, err := supervisor.StagedLaunchCapabilityWithBootstrapLock(ctx, stateRoot)
	if err != nil {
		return err
	}
	return publishPrivateOrdinaryScheduling(stateRoot, supervisor.RuntimeBinding(), capability)
}

func publishPrivateOrdinaryScheduling(stateRoot string, binding task.SupervisorRef, capability pueue.StagedLaunchCapability) error {
	if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return pueue.ErrConfiguration
	}
	if err := validatePrivateOrdinarySchedulingBinding(binding); err != nil {
		return errors.Join(pueue.ErrConfiguration, err)
	}
	if !capability.AddStashed || !capability.EnqueueByID {
		return errors.Join(pueue.ErrConfiguration, pueue.ErrStagedCapability)
	}
	data, err := task.MarshalCanonical(privateOrdinarySchedulingRecord{
		SchemaVersion: task.SchemaVersion,
		Revision:      privateOrdinarySchedulingRevision,
		Supervisor:    binding,
		StagedLaunch:  capability,
	})
	if err != nil {
		return errors.Join(pueue.ErrConfiguration, err)
	}
	return pueue.ReplacePrivateSchedulingEvidence(stateRoot, data)
}
