package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// errInspectionWorkerUnavailable is the only failure returned by the worker.
// Inspection diagnostics can include native or provider-sensitive details, so
// neither those details nor supervisor command errors cross this boundary.
var errInspectionWorkerUnavailable = errors.New("native inspection unavailable")

// runInspectionWorker executes one already-admitted inspection operation. It
// opens only existing durable state, reconstructs the immutable candidate, and
// performs native work inside the same finite supervisor lifetime as its stop
// observer. The worker never records supervisor success; the admission
// observer owns that independent proof.
func runInspectionWorker(root, taskID string, deps Dependencies) (resultErr error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return errInspectionWorkerUnavailable
	}
	canonicalRoot, err := resolveRoot(root)
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	deps = deps.normalized()
	store, err := openStore(canonicalRoot, deps.storeDependencies(), false)
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			resultErr = errInspectionWorkerUnavailable
		}
	}()
	return runInspectionWorkerOperation(canonicalRoot, taskID, deps, store)
}

func runInspectionWorkerOperation(canonicalRoot, taskID string, deps Dependencies, store *taskdir.Store) (resultErr error) {
	operation, err := inspection.LoadOperation(store, taskID)
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			resultErr = errInspectionWorkerUnavailable
		}
	}()

	record := operation.Request()
	meta, err := loadInspectionWorkerMeta(store, taskID)
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	supervisorOptions := supervisorOptionsForCurrentEnvironment(deps.SupervisorOptions)
	observeContext, cancel := context.WithDeadline(context.Background(), operation.Deadline())
	defer cancel()
	restoreEnvironment, err := prepareInspectionWorkerEnvironment(operation, record.Binding)
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	defer func() { resultErr = errors.Join(resultErr, restoreEnvironment()) }()

	supervisor, identity, err := reconcileInspectionWorker(observeContext, canonicalRoot, operation, record.Binding.Supervisor, supervisorOptionsWithEnvironment(supervisorOptions, record.Binding.Environment))
	if err != nil {
		return errInspectionWorkerUnavailable
	}

	startPermit, err := operation.ClaimStart(time.Now())
	if err != nil {
		return errInspectionWorkerUnavailable
	}
	if err = startPermit.Consume(); err != nil {
		return completeInspectionFailure(operation)
	}

	facts, workErr, stopErr := runSupervisedInspection(operation, supervisor, identity, deps, canonicalRoot, record, meta)
	if workErr != nil || stopErr != nil {
		return completeInspectionFailure(operation)
	}

	completedAt := time.Now()
	if operation.Expired(completedAt) {
		return completeInspectionFailure(operation)
	}
	if err = operation.Complete(inspection.ResultEligible, facts, completedAt); err != nil {
		return errInspectionWorkerUnavailable
	}
	return nil
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

func validateInspectionWorkerStartup(operation *inspection.Operation, binding inspection.Binding) error {
	if operation == nil || operation.Expired(time.Now()) {
		return task.ErrEvidenceFault
	}
	return validateCurrentInspectionWorker(binding)
}

func prepareInspectionWorkerEnvironment(operation *inspection.Operation, binding inspection.Binding) (func() error, error) {
	if err := validateInspectionWorkerStartup(operation, binding); err != nil {
		return nil, err
	}
	return applySavedEnvironment(binding.Environment)
}

func reconcileInspectionWorker(ctx context.Context, root string, operation *inspection.Operation, binding task.SupervisorRef, options pueue.Options) (*pueue.Client, pueue.InspectionIdentity, error) {
	supervisor, err := newSupervisorClient(ctx, root, binding, options, true)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	identity, err := inspectionIdentityContext(ctx, operation)
	if err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	observation, err := supervisor.ReconcileInspection(ctx, identity)
	if err != nil || !observation.Matched || observation.Job.State != pueue.StateRunning {
		return nil, pueue.InspectionIdentity{}, errInspectionWorkerUnavailable
	}
	if operation.Expired(time.Now()) {
		return nil, pueue.InspectionIdentity{}, errInspectionWorkerUnavailable
	}
	observedID := observation.Job.ID
	if err = operation.RecordReceiptContext(ctx, observedID); err != nil {
		return nil, pueue.InspectionIdentity{}, err
	}
	identity.NumericTaskID = &observedID
	return supervisor, identity, nil
}

func runSupervisedInspection(operation *inspection.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, deps Dependencies, root string, record inspection.RequestRecord, meta *task.MetaRecord) (json.RawMessage, error, error) {
	stopper := &inspectionBudgetStopper{operation: operation, client: supervisor, identity: identity}
	var facts json.RawMessage
	workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{Stopper: stopper}, func(scope execution.PreflightScope) error {
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
		return err
	}
	if err = validateInspectionCandidate(operation, record.Task, candidate); err != nil {
		return err
	}
	inspected, err := inspection.Inspect(scope, *candidate.Inspection)
	if err != nil {
		return err
	}
	*facts = append((*facts)[:0], inspected...)
	if record.Binding.DefinitionRevision == commonprovider.ModelsRevision {
		return nil
	}
	_, err = finalizeCandidate(candidate, record.Task, *facts)
	return err
}

// validateCurrentInspectionWorker binds this process to the worker executable
// saved when the supervisor job was admitted. The saved digest is never
// replaced with a digest computed from the current process.
func validateCurrentInspectionWorker(binding inspection.Binding) error {
	current, err := os.Executable()
	if err != nil {
		return task.ErrEvidenceFault
	}
	canonical, err := config.CanonicalizePath(current)
	if err != nil || canonical != binding.WorkerExecutable {
		return task.ErrEvidenceFault
	}
	digest, err := commonprovider.FingerprintExecutable(canonical)
	if err != nil || digest != binding.WorkerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

// completeInspectionFailure seals a started operation as unavailable or
// expired, while retaining the fixed worker error for its caller. It is only
// called after the durable start guard exists.
func completeInspectionFailure(operation *inspection.Operation) error {
	if operation == nil {
		return errInspectionWorkerUnavailable
	}
	now := time.Now()
	reason := inspection.ResultUnavailable
	if operation.Expired(now) {
		reason = inspection.ResultExpired
	}
	if err := operation.Complete(reason, nil, now); err != nil {
		return errInspectionWorkerUnavailable
	}
	return errInspectionWorkerUnavailable
}

// inspectionBudgetStopper prepares one durable stop intent at expiry. The
// preparation path performs no supervisor call; the returned request owns the
// external observation and is deliberately never released by the worker.
type inspectionBudgetStopper struct {
	operation *inspection.Operation
	client    *pueue.Client
	identity  pueue.InspectionIdentity
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
		result, stopErr := s.client.StopInspection(requestCtx, permit, s.identity)
		recordErr := recordInspectionStop(requestCtx, s.operation, result)
		return errors.Join(stopErr, recordErr)
	}, nil
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
