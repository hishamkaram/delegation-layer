package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

var ErrRuntimeQuiescenceBusy = errors.New("private runtime is not quiescent")

// CheckRuntimeQuiescence observes root-wide canonical closure before restart
// acceptance. The application owns bootstrap EX and the selected binding;
// this observer grants no shutdown, recovery or task mutation authority.
func CheckRuntimeQuiescence(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) error {
	return checkRuntimeQuiescence(ctx, store, binding, jobs, false)
}

// CheckFencedRuntimeQuiescence also permits exact preparation-only records.
// The caller must first validate the matching durable runtime-restart fence
// under bootstrap EX. These records are not declared admitted or closed.
func CheckFencedRuntimeQuiescence(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) error {
	return checkRuntimeQuiescence(ctx, store, binding, jobs, true)
}

type runtimeCandidates struct {
	tasks       []string
	inspections []string
	workspaces  []string
}

func checkRuntimeQuiescence(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job, fenced bool) error {
	if ctx == nil || store == nil {
		return task.ErrInvalidPermit
	}
	if err := task.ValidateFreshSupervisorRef(binding); err != nil {
		return err
	}
	if !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return task.ErrIdentityMismatch
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	candidates, err := readRuntimeCandidates(ctx, store)
	if err != nil {
		return runtimeQuiescenceError(err)
	}
	if err = validateRuntimeRows(store.RootID, jobs, candidates); err != nil {
		return err
	}
	if err = inspectRuntimeCandidates(ctx, store, candidates, fenced); err != nil {
		return err
	}
	after, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !after.Equal(pin) {
		return task.ErrIdentityMismatch
	}
	return ctx.Err()
}

func readRuntimeCandidates(ctx context.Context, store *taskdir.Store) (runtimeCandidates, error) {
	var result runtimeCandidates
	err := store.WithExistingReadObservation(ctx, func(readContext context.Context) error {
		var readErr error
		result.tasks, readErr = store.TaskIDs(readContext)
		if readErr != nil {
			return readErr
		}
		result.inspections, readErr = store.InspectionOperationIDs(readContext)
		if readErr != nil {
			return readErr
		}
		result.workspaces, readErr = store.WorkspaceOperationIDs(readContext)
		return readErr
	})
	return result, err
}

func validateRuntimeRows(rootID string, jobs []pueue.Job, candidates runtimeCandidates) error {
	labels := PrivateQueueLabelCounts(jobs)
	seen := make(map[int64]bool, len(jobs))
	tasks, auxiliary := candidateSets(candidates)
	for _, job := range jobs {
		if job.ID < 0 || seen[job.ID] {
			return fmt.Errorf("%w: duplicate or invalid private queue identity", task.ErrEvidenceFault)
		}
		seen[job.ID] = true
		switch job.State {
		case pueue.StateEnded:
		case pueue.StateUnknown, pueue.StateQueued, pueue.StateRunning, pueue.StateStashed:
			return ErrRuntimeQuiescenceBusy
		default:
			return fmt.Errorf("%w: invalid private queue state", task.ErrEvidenceFault)
		}
		if job.Label == nil {
			continue
		}
		id, managed, inspectionWorker, err := LegacyJobTaskID(*job.Label, rootID)
		if err != nil {
			return err
		}
		if !managed {
			continue
		}
		present := tasks[id]
		if inspectionWorker {
			present = auxiliary[id]
		}
		if labels[*job.Label] != 1 || !present {
			return fmt.Errorf("%w: private queue label has no unique journal candidate", task.ErrEvidenceFault)
		}
	}
	return nil
}

func candidateSets(candidates runtimeCandidates) (map[string]bool, map[string]bool) {
	tasks := make(map[string]bool, len(candidates.tasks))
	auxiliary := make(map[string]bool, len(candidates.inspections)+len(candidates.workspaces))
	for _, id := range candidates.tasks {
		tasks[id] = true
	}
	for _, id := range candidates.inspections {
		auxiliary[id] = true
	}
	for _, id := range candidates.workspaces {
		auxiliary[id] = true
	}
	return tasks, auxiliary
}

func inspectRuntimeCandidates(ctx context.Context, store *taskdir.Store, candidates runtimeCandidates, fenced bool) error {
	for _, id := range candidates.tasks {
		if err := inspectRuntimeTask(ctx, store, id, fenced); err != nil {
			return fmt.Errorf("runtime quiescence task %s: %w", id, runtimeQuiescenceError(err))
		}
	}
	observe := func(observation taskdir.ControlLifetimeObservation) error {
		return inspectRuntimeAuxiliary(ctx, observation, fenced)
	}
	for _, id := range candidates.inspections {
		if err := inspection.ObserveLifetimeContext(ctx, store, id, observe); err != nil {
			return fmt.Errorf("runtime quiescence inspection %s: %w", id, runtimeQuiescenceError(err))
		}
	}
	for _, id := range candidates.workspaces {
		if err := workspace.ObserveLifetimeContext(ctx, store, id, observe); err != nil {
			return fmt.Errorf("runtime quiescence workspace operation %s: %w", id, runtimeQuiescenceError(err))
		}
	}
	return nil
}

func inspectRuntimeTask(ctx context.Context, store *taskdir.Store, id string, fenced bool) error {
	observation, err := store.ReadTaskExecutionClosureContext(ctx, id)
	if err != nil {
		return err
	}
	if observation.Proof != nil {
		return nil
	}
	if fenced {
		preparationOnly, readErr := store.ReadTaskPreparationOnlyContext(ctx, id)
		if readErr != nil {
			return readErr
		}
		if preparationOnly {
			return nil
		}
	}
	switch observation.PendingReason {
	case taskdir.ConnectorClosureOwnerBusy, taskdir.ConnectorClosureUnproven:
		return ErrRuntimeQuiescenceBusy
	case taskdir.ConnectorClosureMissingOwner, taskdir.ConnectorClosurePartialEvidence:
		return task.ErrEvidenceFault
	}
	return task.ErrEvidenceFault
}

func inspectRuntimeAuxiliary(ctx context.Context, observation taskdir.ControlLifetimeObservation, fenced bool) error {
	_, err := guardian.ReadAuxiliaryClosureProof(ctx, observation.Records)
	if err == nil {
		if !observation.OwnerPresent {
			return fmt.Errorf("%w: auxiliary lifetime owner inode is missing", task.ErrEvidenceFault)
		}
		return nil
	}
	pending := task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, guardian.ErrProofPending) })
	if pending && fenced && observation.PreparationOnly {
		return nil
	}
	return err
}

func runtimeQuiescenceError(err error) error {
	if err == nil || taskdir.CleanupFault(err) {
		return err
	}
	if task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, guardian.ErrProofPending)
	}) {
		return errors.Join(ErrRuntimeQuiescenceBusy, err)
	}
	return err
}
