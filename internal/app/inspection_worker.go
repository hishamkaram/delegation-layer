package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type inspectionWorkerStageError struct {
	stage string
}

func (e *inspectionWorkerStageError) Error() string {
	return admissioncore.ErrInspectionWorkerUnavailable.Error() + ": stage=" + e.stage
}

func (*inspectionWorkerStageError) Unwrap() error {
	return admissioncore.ErrInspectionWorkerUnavailable
}

func inspectionWorkerFailure(stage string) error {
	return &inspectionWorkerStageError{stage: stage}
}

func inspectionWorkerFailureStage(err error, fallback string) string {
	var staged *inspectionWorkerStageError
	if errors.As(err, &staged) && staged.stage != "" {
		return staged.stage
	}
	return fallback
}

func inspectionWorkerPublicError(err error) error {
	var staged *inspectionWorkerStageError
	if errors.As(err, &staged) && staged.stage != "" {
		return staged
	}
	return admissioncore.ErrInspectionWorkerUnavailable
}

func runInspectionWorkerMode(root, taskID string, supervised bool, deps Dependencies) (resultErr error) {
	return runInspectionWorkerWithLifetime(root, taskID, supervised, nil, deps)
}

func runInspectionLifetimeChildMode(parsed runnerArguments, deps Dependencies) (resultErr error) {
	return runInspectionWorkerWithLifetime(parsed.Root, parsed.TaskID, parsed.Supervised, &parsed, deps)
}

func runInspectionWorkerWithLifetime(root, taskID string, supervised bool, lifetimeArgs *runnerArguments, deps Dependencies) (resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return inspectionWorkerFailure("validate-task")
	}
	canonicalRoot, err := tasklifecycle.ResolveRoot(root)
	if err != nil {
		return inspectionWorkerFailure("resolve-root")
	}
	deps = deps.normalized()
	store, err := openStore(canonicalRoot, deps.storeDependencies(), false)
	if err != nil {
		return inspectionWorkerFailure("open-store")
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil && resultErr == nil {
			resultErr = inspectionWorkerFailure("close-store")
		}
	}()
	return runInspectionWorkerOperationModeWithLifetime(canonicalRoot, taskID, supervised, lifetimeArgs, deps, store)
}

func runInspectionWorkerOperationModeWithLifetime(canonicalRoot, taskID string, supervised bool, lifetimeArgs *runnerArguments, deps Dependencies, store *taskdir.Store) (resultErr error) {
	operation, err := loadInspectionOperationForWorker(store, taskID)
	if err != nil {
		return inspectionWorkerFailure("load-operation")
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil && resultErr == nil {
			resultErr = inspectionWorkerFailure("close-operation")
		}
	}()
	lifetimeChild, err := openInspectionLifetimeChildIfRequested(lifetimeArgs, operation)
	if err != nil {
		return inspectionWorkerFailure("open-lifetime")
	}
	if lifetimeChild != nil {
		defer func() {
			if closeErr := lifetimeChild.Close(); closeErr != nil && resultErr == nil {
				resultErr = inspectionWorkerFailure("close-lifetime")
			}
		}()
	}
	return runLoadedInspectionWorkerOperation(canonicalRoot, taskID, supervised, lifetimeChild, deps, store, operation)
}

func loadInspectionOperationForWorker(store *taskdir.Store, taskID string) (*inspection.Operation, error) {
	// Loading cannot know the persisted revision until request.json is read.
	// Use the largest supported inspection window, then let the loaded immutable
	// deadline govern every action that follows.
	ctx, cancel := context.WithTimeout(context.Background(), inspection.ModelDiscoveryTimeout)
	defer cancel()
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return nil, err
	}
	identity, err := admissioncore.OperationID(ctx, store, operation.Request().Task)
	if err != nil || identity != operation.Request().IdentityID() {
		return nil, errors.Join(task.ErrIdentityMismatch, err, operation.Close())
	}
	return operation, nil
}

func openInspectionLifetimeChildIfRequested(lifetimeArgs *runnerArguments, operation *inspection.Operation) (*openedLifetimeChild, error) {
	if lifetimeArgs == nil {
		return nil, nil
	}
	return openInspectionLifetimeChild(*lifetimeArgs, operation)
}

func runLoadedInspectionWorkerOperation(canonicalRoot, taskID string, supervised bool, lifetimeChild *openedLifetimeChild, deps Dependencies, store *taskdir.Store, operation *inspection.Operation) (resultErr error) {
	record := operation.Request()
	if record.Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 && lifetimeChild == nil {
		return inspectionWorkerFailure("open-lifetime")
	}
	meta, err := loadInspectionWorkerMeta(store, record.TaskID)
	if err != nil {
		return inspectionWorkerFailure("load-task")
	}
	supervisorOptions := supervisorcore.OptionsForCurrentEnvironment(deps.SupervisorOptions)
	observeContext, cancel := context.WithDeadline(context.Background(), operation.Deadline())
	defer cancel()
	restoreEnvironment, err := prepareInspectionWorkerEnvironment(observeContext, canonicalRoot, operation, record.Binding)
	if err != nil {
		return inspectionWorkerFailure("prepare-environment")
	}
	defer func() { resultErr = errors.Join(resultErr, restoreEnvironment()) }()

	workerOptions := supervisorcore.OptionsWithEnvironment(supervisorOptions, record.Binding.Environment)
	supervisor, identity, err := inspectionWorkerSupervisor(observeContext, operation, supervised, record.Binding, workerOptions)
	if err != nil {
		return inspectionWorkerFailure("resume-supervisor")
	}
	var maintenanceErr error
	defer func() { supervisorcore.CloseMaintenance(supervisor, errors.Join(resultErr, maintenanceErr)) }()

	startPermit, err := operation.ClaimStartContext(observeContext, time.Now())
	if err != nil {
		return inspectionWorkerFailure("claim-start")
	}
	if err = startPermit.Consume(); err != nil {
		return completeInspectionFailureContext(observeContext, operation, "consume-start")
	}

	facts, workErr, stopErr := runSupervisedInspection(operation, supervisor, identity, lifetimeChild.Lifetime(), deps, canonicalRoot, record, meta)
	maintenanceErr = errors.Join(workErr, stopErr)
	if workErr != nil || stopErr != nil {
		stage := inspectionWorkerFailureStage(workErr, "run-inspection")
		if stopErr != nil {
			stage = "stop-inspection"
		}
		return completeInspectionFailureContext(observeContext, operation, stage)
	}

	completedAt := time.Now()
	if operation.Expired(completedAt) {
		return completeInspectionFailureAt(operation, "inspection-expired")
	}
	if err = operation.CompleteContext(observeContext, inspection.ResultEligible, facts, completedAt); err != nil {
		return inspectionWorkerFailure("complete-success")
	}
	return nil
}

func inspectionWorkerSupervisor(ctx context.Context, operation *inspection.Operation, supervised bool, binding inspection.Binding, options pueue.Options) (*pueue.Client, pueue.InspectionIdentity, error) {
	// The argv marker is not launch authority by itself. Only the immutable
	// managed binding may consume the add receipt; custom and historical
	// bindings must prove their exact running row before they can claim start.
	if supervised && binding.RunnerOwnership == task.RunnerOwnershipManaged {
		return resumeSupervisedInspectionWorker(ctx, operation, binding.Supervisor, options)
	}
	return reconcileInspectionWorker(ctx, operation, binding.Supervisor, options)
}

func loadInspectionWorkerMeta(store *taskdir.Store, taskID string) (meta *task.MetaRecord, resultErr error) {
	td, err := store.OpenTask(taskID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, meta, resultErr = td.PreparedRecords()
	return meta, resultErr
}

func validateInspectionWorkerStartup(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	if operation == nil || operation.Expired(time.Now()) {
		return task.ErrEvidenceFault
	}
	return validateCurrentInspectionWorker(ctx, root, operation, binding)
}

func prepareInspectionWorkerEnvironment(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) (func() error, error) {
	if err := validateInspectionWorkerStartup(ctx, root, operation, binding); err != nil {
		return nil, err
	}
	return applySavedEnvironment(binding.Environment, binding.EnvironmentRecorded)
}

func reconcileInspectionWorker(ctx context.Context, operation *inspection.Operation, binding task.SupervisorRef, options pueue.Options) (supervisor *pueue.Client, identity pueue.InspectionIdentity, resultErr error) {
	// This worker is already a task started by the saved private supervisor.
	// Reopening that supervisor for lifecycle recovery would make every worker
	// contend for bootstrap/readiness controls and could never safely restart
	// the process that launched it. The worker only verifies and observes its
	// own recorded row; admission and status own recovery.
	supervisor, err := pueue.NewClient(binding, options)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	defer func(owner *pueue.Client) {
		if resultErr != nil {
			supervisorcore.CloseMaintenance(owner, resultErr)
			supervisor = nil
		}
	}(supervisor)
	identity, err = admissioncore.InspectionIdentityContext(ctx, operation)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	// Admission and worker startup observe the same inspection deadline. Keep
	// the original client for stop control and ownership of migration cleanup.
	observer, err := supervisor.WithObservationTimeout(inspection.TimeoutForRevision(operation.Request().Binding.DefinitionRevision))
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	observation, err := observer.ReconcileInspection(ctx, identity)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	if !observation.Matched || observation.Job.State != pueue.StateRunning {
		return nil, pueue.InspectionIdentity{}, admissioncore.ErrInspectionWorkerUnavailable
	}
	if operation.Expired(time.Now()) {
		return nil, pueue.InspectionIdentity{}, admissioncore.ErrInspectionWorkerUnavailable
	}
	observedID := observation.Job.ID
	if err = admissioncore.RequireInspectionReceipt(ctx, operation, observedID); err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	identity.NumericTaskID = &observedID
	return supervisor, identity, nil
}

// resumeSupervisedInspectionWorker consumes the numeric receipt persisted by
// the admission owner after Pueue accepted this exact immutable operation.
// The supervisor already started this process, so another queue-wide status
// scan cannot add launch authority and makes concurrent admission scale with
// the whole queue. Markerless custom and legacy workers retain conservative
// queue reconciliation through reconcileInspectionWorker.
func resumeSupervisedInspectionWorker(ctx context.Context, operation *inspection.Operation, binding task.SupervisorRef, options pueue.Options) (*pueue.Client, pueue.InspectionIdentity, error) {
	if operation == nil {
		return nil, pueue.InspectionIdentity{}, task.ErrEvidenceFault
	}
	receipt, err := waitInspectionSupervisorReceipt(ctx, operation)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	supervisor, err := pueue.NewClient(binding, options)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	identity := operation.Request().SupervisorIdentity()
	numericTaskID := *receipt.NumericTaskID
	identity.NumericTaskID = &numericTaskID
	return supervisor, identity, nil
}

func waitInspectionSupervisorReceipt(ctx context.Context, operation *inspection.Operation) (inspection.ReceiptRecord, error) {
	if ctx == nil || operation == nil {
		return inspection.ReceiptRecord{}, task.ErrEvidenceFault
	}
	for {
		receipt, err := operation.ReceiptContext(ctx)
		if err == nil {
			return receipt, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return inspection.ReceiptRecord{}, err
		}
		timer := time.NewTimer(runnerReceiptPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return inspection.ReceiptRecord{}, errors.Join(inspection.ErrAdmissionExpired, ctx.Err())
		case <-timer.C:
		}
	}
}

func runSupervisedInspection(operation *inspection.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, lifetime execution.Lifetime, deps Dependencies, root string, record inspection.RequestRecord, meta *task.MetaRecord) (json.RawMessage, error, error) {
	stopper := &inspectionBudgetStopper{operation: operation, client: supervisor, identity: identity, lifetime: lifetime}
	var facts json.RawMessage
	workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{Stopper: stopper, Lifetime: lifetime}, func(scope execution.PreflightScope) error {
		return runInspectionCallback(scope, operation, deps, root, record, meta, &facts)
	})
	return facts, workErr, stopErr
}

func runInspectionCallback(scope execution.PreflightScope, operation *inspection.Operation, deps Dependencies, root string, record inspection.RequestRecord, meta *task.MetaRecord, facts *json.RawMessage) error {
	// Candidate preparation may perform the compiled attributes-only native
	// lookup. Keep it inside the armed lifetime so expiry owns its stop
	// boundary before any such work can block the worker.
	var candidate commonprovider.ProfileCandidate
	var err error
	if record.Binding.DefinitionRevision == commonprovider.ModelsRevision {
		candidate, err = prepareModelsCandidate(deps, root, record.Task)
	} else {
		candidate, err = prepareInspectionWorkerCandidate(deps, root, record.Task, meta)
	}
	if err != nil {
		return inspectionWorkerFailure("prepare-candidate")
	}
	if err = admissioncore.ValidateInspectionCandidate(scope.Context(), operation, record.Task, candidate); err != nil {
		return inspectionWorkerFailure("validate-candidate")
	}
	inspected, err := inspection.Inspect(scope, *candidate.Inspection)
	if err != nil {
		return inspectionWorkerFailure("inspect-provider")
	}
	*facts = append((*facts)[:0], inspected...)
	if record.Binding.DefinitionRevision == commonprovider.ModelsRevision {
		return nil
	}
	_, err = admissioncore.FinalizeCandidate(candidate, record.Task, *facts)
	if err != nil {
		return inspectionWorkerFailure("finalize-candidate")
	}
	return nil
}

// validateCurrentInspectionWorker accepts the immutable worker identity or a
// digest-verified state-root runner authorized by a durable managed upgrade.
func validateCurrentInspectionWorker(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	current, err := os.Executable()
	if err != nil {
		return task.ErrEvidenceFault
	}
	return validateInspectionWorkerExecutable(ctx, root, operation, binding, current)
}

func validateInspectionWorkerExecutable(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding, current string) error {
	if operation == nil {
		return task.ErrEvidenceFault
	}
	if ctx == nil {
		return context.Canceled
	}
	saved := operation.Request().Binding
	if !sameInspectionWorkerBinding(saved, binding) {
		return task.ErrEvidenceFault
	}
	canonical, err := config.CanonicalizePath(current)
	if err != nil {
		return task.ErrEvidenceFault
	}
	digest, err := commonprovider.FingerprintExecutable(canonical)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if canonical == binding.WorkerExecutable && digest == binding.WorkerSHA256 {
		return nil
	}
	if !managedInspectionWorkerBinding(saved, binding) {
		return task.ErrEvidenceFault
	}
	if !supervisorcore.IsStateRunnerPath(root, canonical) || !supervisorcore.CurrentManagedRunnerExecutable(root, canonical) {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil {
		return err
	}
	if !authorized {
		return task.ErrEvidenceFault
	}
	return nil
}

func sameInspectionWorkerBinding(saved, current inspection.Binding) bool {
	return saved.WorkerExecutable == current.WorkerExecutable &&
		saved.WorkerSHA256 == current.WorkerSHA256 &&
		saved.RunnerOwnership == current.RunnerOwnership &&
		saved.WorkerExitProof == current.WorkerExitProof
}

func managedInspectionWorkerBinding(saved, binding inspection.Binding) bool {
	return saved.RunnerOwnership == task.RunnerOwnershipManaged && binding.RunnerOwnership == task.RunnerOwnershipManaged
}

func completeInspectionFailureAt(operation *inspection.Operation, stage string) error {
	if operation == nil {
		return inspectionWorkerFailure(stage)
	}
	now := time.Now()
	reason := inspection.ResultUnavailable
	if operation.Expired(now) {
		reason = inspection.ResultExpired
	}
	if err := operation.Complete(reason, nil, now); err != nil {
		return inspectionWorkerFailure("complete-failure")
	}
	return inspectionWorkerFailure(stage)
}

func completeInspectionFailureContext(ctx context.Context, operation *inspection.Operation, stage string) error {
	if operation == nil || ctx == nil {
		return inspectionWorkerFailure(stage)
	}
	now := time.Now()
	reason := inspection.ResultUnavailable
	if operation.Expired(now) {
		reason = inspection.ResultExpired
		// Expiry cancels the worker's observation context, but the worker still
		// owns sealing that already-observed terminal fact. WithoutCancel keeps
		// values while taskdir applies the single bounded persistence fallback.
		ctx = context.WithoutCancel(ctx)
	}
	if err := operation.CompleteContext(ctx, reason, nil, now); err != nil {
		return inspectionWorkerFailure("complete-failure")
	}
	return inspectionWorkerFailure(stage)
}

// inspectionBudgetStopper prepares one durable stop intent at expiry. The
// preparation path performs no supervisor call; the returned request owns the
// external observation and is deliberately never released by the worker.
type inspectionBudgetStopper struct {
	operation *inspection.Operation
	client    *pueue.Client
	identity  pueue.InspectionIdentity
	lifetime  execution.Lifetime
}

func (s *inspectionBudgetStopper) PrepareBudget(deadline time.Time) (execution.BudgetRequest, error) {
	if s == nil || s.operation == nil || s.client == nil {
		return nil, task.ErrInvalidPermit
	}
	permit, _, err := s.operation.ClaimStop(deadline)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		requestCtx := execution.DetachBudgetRequestContext(ctx)
		if s.lifetime != nil {
			return s.stopLifetime(requestCtx, permit)
		}
		result, stopErr := s.client.StopInspection(requestCtx, permit, s.identity)
		recordErr := recordInspectionStop(requestCtx, s.operation, result)
		return errors.Join(stopErr, recordErr)
	}, nil
}

func (s *inspectionBudgetStopper) stopLifetime(ctx context.Context, permit *inspection.StopPermit) error {
	if permit == nil {
		return task.ErrInvalidPermit
	}
	if err := permit.Consume(); err != nil {
		return err
	}
	if err := s.lifetime.Terminate(ctx); err != nil {
		return err
	}
	numericTaskID := permit.InspectionStopNumericTaskID()
	if numericTaskID == nil {
		return task.ErrEvidenceFault
	}
	// The guardian acknowledgment proves the native lifetime is empty. The
	// existing inspection observer remains the sole owner of the separate
	// Pueue-ended observation after this worker exits.
	return s.operation.RecordStopReplyContext(ctx, *numericTaskID, "terminate", true)
}

// recordInspectionStop keeps acknowledgment and termination as separate
// durable facts. Only an explicit command acknowledgment is written as a
// reply; an observation is written only for a fresh ended-state observation.
func recordInspectionStop(ctx context.Context, operation *inspection.Operation, result pueue.StopResult) error {
	if ctx == nil || operation == nil {
		return task.ErrEvidenceFault
	}
	var resultErr error
	if result.Acknowledged != nil {
		if result.NumericTaskID == nil {
			return task.ErrEvidenceFault
		}
		resultErr = errors.Join(resultErr, operation.RecordStopReplyContext(ctx, *result.NumericTaskID, result.Action, *result.Acknowledged))
	}
	if result.ObservedState == pueue.StateEnded {
		if result.NumericTaskID == nil {
			return errors.Join(resultErr, task.ErrEvidenceFault)
		}
		resultErr = errors.Join(resultErr, operation.RecordStopObservationContext(ctx, *result.NumericTaskID))
	}
	return resultErr
}
