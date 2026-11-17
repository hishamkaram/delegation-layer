package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
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

// RuntimeMaintenanceError identifies the canonical resource that blocks
// maintenance without parsing formatted error text or exposing a pathname.
type RuntimeMaintenanceError struct {
	Kind  string
	ID    string
	Cause error
}

func (e *RuntimeMaintenanceError) Error() string {
	return "runtime maintenance " + e.Kind + " " + e.ID + ": " + e.Cause.Error()
}
func (e *RuntimeMaintenanceError) Unwrap() error { return e.Cause }

// CheckMaintenanceQuiescence derives its preparation-only rule from the
// canonical exact-root Drain under the actual generation SH or caller's EX.
// No caller-provided readiness flag can enable the narrower inspection.
func CheckMaintenanceQuiescence(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) error {
	if ctx == nil || store == nil {
		return task.ErrInvalidPermit
	}
	return withMaintenanceLease(ctx, store, func(locked context.Context) error {
		return checkMaintenanceQuiescence(locked, store, binding, jobs)
	})
}

func withMaintenanceLease(ctx context.Context, store *taskdir.Store, inspect func(context.Context) error) error {
	if ctx == nil || store == nil {
		return task.ErrInvalidPermit
	}
	if pueue.RequireRuntimeRestartBootstrap(ctx, store.Root) == nil {
		return inspect(ctx)
	}
	return pueue.WithSupervisorLeaseContext(ctx, store.Root, inspect)
}

func checkMaintenanceQuiescence(locked context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) error {
	pin, err := store.PinRootContext(locked)
	if err != nil {
		return err
	}
	observed, found, err := pueue.ObservePrivateUpgrade(locked, pin, "")
	if err != nil {
		return err
	}
	if found && (observed.Completed || observed.Predecessor != binding) {
		return task.ErrIdentityMismatch
	}
	return checkRuntimeQuiescence(locked, store, binding, jobs, found)
}

// CheckRuntimeRestartQuiescence retains the explicit restart subject. The
// canonical reader, not the callback, validates the exact pending fence before
// preparation-only records can be considered for maintenance.
func CheckRuntimeRestartQuiescence(ctx context.Context, store *taskdir.Store, from pueue.RuntimeRestartReference, binding task.SupervisorRef, jobs []pueue.Job) error {
	if ctx == nil || store == nil {
		return task.ErrInvalidPermit
	}
	if err := pueue.RequireRuntimeRestartBootstrap(ctx, store.Root); err != nil {
		return err
	}
	pending, err := pueue.PrivateRuntimeRestartPending(ctx, store.Root)
	if err != nil || !pending {
		if err != nil {
			return err
		}
		return CheckRuntimeQuiescence(ctx, store, binding, jobs)
	}
	observed, err := pueue.ObservePrivateRuntimeRestart(ctx, store.Root, from)
	if err != nil {
		return err
	}
	if observed.From != from || observed.Binding != binding || observed.Phase == pueue.RuntimeRestartReady {
		return task.ErrIdentityMismatch
	}
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
	if err = inspectRuntimeCandidates(ctx, store, candidates, fenced, binding, jobs); err != nil {
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

func inspectRuntimeCandidates(ctx context.Context, store *taskdir.Store, candidates runtimeCandidates, fenced bool, binding task.SupervisorRef, jobs []pueue.Job) error {
	var resultErr error
	for _, id := range candidates.tasks {
		if err := inspectRuntimeTask(ctx, store, id, fenced, binding, jobs); err != nil {
			resultErr = joinRuntimeObservation(resultErr, &RuntimeMaintenanceError{Kind: "task", ID: id, Cause: runtimeQuiescenceError(err)})
		}
	}
	observe := func(observation taskdir.ControlLifetimeObservation) error {
		return inspectRuntimeAuxiliary(ctx, observation, fenced)
	}
	for _, id := range candidates.inspections {
		if err := inspectRuntimeInspection(ctx, store, id, fenced, binding, jobs); err != nil {
			resultErr = joinRuntimeObservation(resultErr, &RuntimeMaintenanceError{Kind: "inspection", ID: id, Cause: runtimeQuiescenceError(err)})
		}
	}
	for _, id := range candidates.workspaces {
		if err := workspace.ObserveLifetimeContext(ctx, store, id, observe); err != nil {
			resultErr = joinRuntimeObservation(resultErr, &RuntimeMaintenanceError{Kind: "workspace_operation", ID: id, Cause: runtimeQuiescenceError(err)})
		}
	}
	return resultErr
}

func inspectRuntimeTask(ctx context.Context, store *taskdir.Store, id string, fenced bool, binding task.SupervisorRef, jobs []pueue.Job) error {
	observation, err := store.ReadTaskExecutionClosureContext(ctx, id)
	if err != nil {
		return err
	}
	if observation.Proof != nil {
		return nil
	}
	allowed, permissionErr := inspectEndedTaskPermission(ctx, store, id, binding, jobs)
	if allowed || permissionErr != nil {
		return permissionErr
	}
	if err := inspectTaskAdmissionGuidance(ctx, store, id); err != nil {
		return err
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

// Prefer fault metadata over a busy sibling while retaining every cause.
func joinRuntimeObservation(previous, current error) error {
	if current == nil {
		return previous
	}
	if !MaintenanceBusy(current) {
		return errors.Join(current, previous)
	}
	return errors.Join(previous, current)
}

func inspectRuntimeInspection(ctx context.Context, store *taskdir.Store, id string, fenced bool, binding task.SupervisorRef, jobs []pueue.Job) error {
	var saved task.SupervisorRef
	var label string
	var permissionNeeded bool
	var queueValidated bool
	observe := func(observation inspection.MaintenanceObservation) error {
		required, err := inspectInspectionObservation(ctx, id, fenced, queueValidated, observation)
		if required {
			saved, label, permissionNeeded = observation.Supervisor, observation.Label, true
		}
		return err
	}
	err := inspection.ObserveMaintenanceContext(ctx, store, id, inspection.MaintenanceQueue{}, lifetime.NativeObservation{}, observe)
	if err != nil || !permissionNeeded {
		return err
	}
	queue, err := ObserveInspectionMaintenanceQueue(ctx, store, saved, binding, label, jobs)
	if err != nil {
		return err
	}
	queueValidated = true
	return inspection.ObserveMaintenanceContext(ctx, store, id, queue, lifetime.NativeObservation{}, observe)
}

func auxiliaryMaintenancePending(kind, id, reason, action string) error {
	assessment := MaintenanceAssessment{Readiness: "blocked", ActiveWork: "none", FirstBlocker: &MaintenanceBlocker{Kind: kind, OperationID: id, ReasonCode: reason, NextAction: action}}
	return &MaintenanceError{Assessment: assessment, Cause: ErrRuntimeQuiescenceBusy}
}

func inspectTaskAdmissionGuidance(ctx context.Context, store *taskdir.Store, id string) error {
	td, openErr := store.OpenTask(id)
	if openErr != nil {
		return openErr
	}
	admissionOnly, admissionErr := td.ReadAdmissionOnlyContext(ctx)
	admissionErr = errors.Join(admissionErr, td.Close())
	if admissionErr != nil {
		return admissionErr
	}
	if admissionOnly {
		assessment := MaintenanceAssessment{Readiness: "blocked", ActiveWork: "none", FirstBlocker: &MaintenanceBlocker{Kind: "task", TaskID: id, ReasonCode: "admission_only_unlaunched", NextAction: "cancel_task"}}
		return &MaintenanceError{Assessment: assessment, Cause: ErrRuntimeQuiescenceBusy}
	}
	return nil
}

func inspectInspectionObservation(ctx context.Context, id string, fenced, queueValidated bool, observation inspection.MaintenanceObservation) (bool, error) {
	if observation.HistoricalExit || observation.Permission {
		return false, nil
	}
	err := inspectRuntimeAuxiliary(ctx, observation.Lifetime, false)
	if err == nil || !errors.Is(err, guardian.ErrProofPending) {
		return false, err
	}
	if observation.RecoveryPending {
		return false, err
	}
	if observation.Disposition != nil {
		if queueValidated {
			return false, task.ErrEvidenceFault
		}
		return true, nil
	}
	if observation.Lifetime.PreparationOnly {
		if !fenced {
			return false, err
		}
		reason, action := "recovery_required", "run_supervisor_ensure_owner"
		if time.Now().Before(observation.Deadline) {
			reason, action = "awaiting_deadline", "wait_for_legacy_work"
		}
		return false, auxiliaryMaintenancePending("inspection", id, reason, action)
	}
	if observation.SettlementEligible {
		if !queueValidated {
			return true, nil
		}
		return false, auxiliaryMaintenancePending("inspection", id, "inspection_closure_unproven", "settle_inspection")
	}
	return false, &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "evidence", Name: "worker-observation.json", ReasonCode: "inspection_exit_unobservable", NextAction: "human_review_operation", Cause: task.ErrEvidenceFault}
}

func inspectEndedTaskPermission(ctx context.Context, store *taskdir.Store, id string, binding task.SupervisorRef, jobs []pueue.Job) (bool, error) {
	for _, job := range jobs {
		if job.Label == nil || *job.Label != "delegate:"+store.RootID+":"+id || job.State != pueue.StateEnded {
			continue
		}
		td, openErr := store.OpenTask(id)
		if openErr != nil {
			return false, errors.Join(task.ErrEvidenceFault, openErr)
		}
		allowed, dispositionErr := inspectTaskMaintenancePermission(ctx, store, td, binding, job)
		dispositionErr = errors.Join(dispositionErr, td.Close())
		if allowed || dispositionErr != nil {
			return allowed, dispositionErr
		}
	}
	return false, nil
}
