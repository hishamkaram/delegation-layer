package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type admissionPreparation struct {
	Profile            PreparedProfile
	Supervisor         *pueue.Client
	InspectionDeadline time.Time
	Capability         CapabilityReport
}

// supervisorOptionsForCandidate carries the provider's bounded, nonsecret
// launch environment into the supervisor client used for inspection and the
// ordinary task. The inspection worker is started by that supervisor and
// reconstructs the candidate from its process environment; using the same
// values here keeps its immutable definition identical to admission. The
// caller's explicit supervisor environment remains authoritative for keys it
// does not share with the provider profile.
func supervisorOptionsForCandidate(base pueue.Options, candidate commonprovider.ProfileCandidate) (pueue.Options, error) {
	if candidate.Inspection == nil {
		return base, nil
	}
	definition, _, err := candidate.Inspection.Snapshot()
	if err != nil {
		return pueue.Options{}, err
	}
	return supervisorOptionsWithEnvironment(base, definition.Environment), nil
}

// supervisorOptionsForProfile is the retry/recovery counterpart of
// supervisorOptionsForCandidate. A task that was admitted but whose ordinary
// submission must be retried still needs the exact provider environment that
// was used to construct its immutable plan.
func supervisorOptionsForProfile(base pueue.Options, profile PreparedProfile) pueue.Options {
	return supervisorOptionsWithEnvironment(base, profile.Plan.Environment)
}

func supervisorOptionsWithEnvironment(base pueue.Options, providerEnvironment []string) pueue.Options {
	if len(providerEnvironment) == 0 {
		return base
	}
	var merged []string
	if base.Environment == nil {
		merged = pueue.DefaultEnvironment()
	} else {
		// Preserve the caller's explicit empty environment. A nil slice means
		// "use the client default"; a non-nil empty slice intentionally means
		// "launch without inherited variables".
		merged = make([]string, len(base.Environment))
		copy(merged, base.Environment)
	}
	positions := make(map[string]int, len(merged)+len(providerEnvironment))
	for index, entry := range merged {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			positions[key] = index
		}
	}
	for _, entry := range providerEnvironment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if index, exists := positions[key]; exists {
			merged[index] = entry
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, entry)
	}
	base.Environment = merged
	return base
}

func prepareAdmission(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord) (admissionPreparation, error) {
	candidate, err := prepareCandidate(deps, store.Root, req)
	if err != nil {
		return admissionPreparation{}, err
	}
	var profile PreparedProfile
	if candidate.Inspection == nil {
		profile, err = finalizeCandidate(candidate, req, nil)
		if err != nil {
			return admissionPreparation{}, err
		}
	}
	supervisorOptions, err := supervisorOptionsForCandidate(deps.SupervisorOptions, candidate)
	if err != nil {
		return admissionPreparation{}, err
	}
	supervisor, err := bindInitialWithOptions(a, deps, store.Root, supervisorOptions)
	if err != nil {
		return admissionPreparation{}, err
	}
	prepared := admissionPreparation{Profile: profile, Supervisor: supervisor}
	if candidate.Inspection != nil {
		facts, deadline, inspectionErr := admissionInspectionFacts(a, deps, store, req, candidate, supervisor)
		if inspectionErr != nil {
			return admissionPreparation{}, inspectionErr
		}
		prepared.Profile, err = finalizeCandidate(candidate, req, facts)
		if err != nil {
			return admissionPreparation{}, err
		}
		prepared.InspectionDeadline = deadline
	}
	prepared.Capability, err = runtimeCapability(req.Provider, candidate, prepared.Profile)
	if err != nil {
		return admissionPreparation{}, err
	}
	if err = prepared.Profile.ValidateStatePlacement(store.Root); err != nil {
		return admissionPreparation{}, err
	}
	return prepared, nil
}

func ensureInspectionGroup(store *taskdir.Store, supervisor *pueue.Client) (resultErr error) {
	group, err := inspection.OpenGroup(store, supervisor.Binding())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), inspection.AdmissionTimeout)
	defer cancel()
	snapshot, err := supervisor.Snapshot(ctx)
	if err != nil {
		joinInspectionPending(inFlightInspectionError(err))
		return err
	}
	if _, exists := snapshot.Groups[group.Name()]; exists {
		return group.Observe(snapshot)
	}
	permit, err := group.ClaimCreateContext(ctx, time.Now())
	if err != nil {
		return err
	}
	_, createErr := supervisor.CreateInspectionGroup(ctx, permit, store.RootID)
	if pending := inFlightInspectionError(createErr); pending != nil {
		// The create command owns a live process even though observation ended.
		// Join it before returning; the saved guard prevents a later retry.
		joinInspectionPending(pending)
		// The create response was uncertain, so one fresh snapshot may resolve
		// the durable intent. Do not start that observation after the bounded
		// bootstrap context has expired: the original in-flight result remains
		// the only authority in that case.
		if ctx.Err() != nil {
			return createErr
		}
	}
	// A failed or lost response never permits another create. A fresh exact
	// snapshot can resolve the saved intent, including after an uncertain reply.
	if err = ctx.Err(); err != nil {
		return errors.Join(createErr, err)
	}
	snapshot, observeErr := supervisor.Snapshot(ctx)
	if observeErr != nil {
		joinInspectionPending(inFlightInspectionError(observeErr))
		return errors.Join(createErr, observeErr)
	}
	if observeErr = group.Observe(snapshot); observeErr != nil {
		return errors.Join(createErr, observeErr)
	}
	return nil
}

func admissionInspectionFacts(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (facts json.RawMessage, deadline time.Time, resultErr error) {
	runner, err := resolveRunner(a.Runner, deps)
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("resolving worker", err)
	}
	workerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("fingerprinting worker", err)
	}
	definition, definitionSHA, err := candidate.Inspection.Snapshot()
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("snapshotting definition", err)
	}
	if err = ensureInspectionGroup(store, supervisor); err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("ensuring inspection group", err)
	}
	operation, err := inspection.OpenOperation(store, req, inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: runner, WorkerSHA256: workerSHA, Supervisor: supervisor.Binding(),
	}, time.Now())
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("opening inspection operation", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Expired(time.Now()) {
		return nil, operation.Deadline(), inspection.ErrAdmissionExpired
	}
	ctx, cancel := context.WithDeadline(context.Background(), operation.Deadline())
	defer cancel()
	if err = submitInspectionOnce(ctx, operation, supervisor, runner, store.Root); err != nil {
		return nil, operation.Deadline(), annotateMissingInspectionStage("submitting inspection", err)
	}
	facts, err = awaitInspection(ctx, operation, supervisor)
	if err != nil {
		return nil, operation.Deadline(), annotateMissingInspectionStage("awaiting inspection", err)
	}
	if operation.Expired(time.Now()) {
		return nil, operation.Deadline(), inspection.ErrAdmissionExpired
	}
	return facts, operation.Deadline(), nil
}

// annotateMissingInspectionStage gives a missing-file failure a fixed stage
// without exposing paths, records, or native diagnostics. The original error
// remains wrapped so callers can still classify errors.Is(os.ErrNotExist).
func annotateMissingInspectionStage(stage string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("inspection %s: %w", stage, err)
}

func submitInspectionOnce(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, runner, root string) error {
	permit, err := operation.ClaimSubmissionContext(ctx, time.Now())
	if errors.Is(err, task.ErrAlreadySubmitted) {
		return nil
	}
	if err != nil {
		return err
	}
	record := operation.Request()
	// The worker executable is part of the immutable inspection binding. It was
	// fingerprinted before the durable claim, so revalidate the exact path and
	// bytes immediately before handing the launch to the supervisor. A changed
	// worker consumes the create-once claim but can never reach an external add.
	if err = validateInspectionRunner(operation, runner); err != nil {
		return err
	}
	observation, submitErr := supervisor.SubmitInspection(ctx, permit, pueue.InspectionIdentity{RootID: record.RootID, TaskID: record.TaskID}, pueue.Launch{RunnerExecutable: runner, RootPath: root})
	if pending := inFlightInspectionError(submitErr); pending != nil {
		// SubmitInspection may have timed out in its verification, add, or
		// reconciliation command. No follow-up status may race that process.
		joinInspectionPending(pending)
		return submitErr
	}
	if observation.Pending != nil {
		joinInspectionPending(observation.Pending)
		if submitErr == nil {
			return pueue.ErrInFlight
		}
		return submitErr
	}
	if observation.Matched {
		return operation.RecordReceiptContext(ctx, observation.Job.ID)
	}
	if ctx.Err() != nil {
		return errors.Join(submitErr, ctx.Err())
	}
	// The durable guard is consumed even if add completion is unknown. Only
	// observational reconciliation follows; no failure path resubmits.
	return nil
}

func validateInspectionRunner(operation *inspection.Operation, runner string) error {
	if operation == nil || runner == "" {
		return task.ErrEvidenceFault
	}
	binding := operation.Request().Binding
	if runner != binding.WorkerExecutable {
		return task.ErrIdentityMismatch
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil || digest != binding.WorkerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func inspectionIdentity(operation *inspection.Operation) (pueue.InspectionIdentity, error) {
	return inspectionIdentityContext(context.Background(), operation)
}

func inspectionIdentityContext(ctx context.Context, operation *inspection.Operation) (pueue.InspectionIdentity, error) {
	record := operation.Request()
	identity := pueue.InspectionIdentity{RootID: record.RootID, TaskID: record.TaskID}
	receipt, err := operation.ReceiptContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return identity, nil
	}
	if err != nil {
		return identity, err
	}
	identity.NumericTaskID = receipt.NumericTaskID
	return identity, nil
}

func awaitInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (json.RawMessage, error) {
	identity, err := inspectionIdentityContext(ctx, operation)
	if err != nil {
		return nil, err
	}
	for {
		if ctx.Err() != nil {
			return nil, inspection.ErrAdmissionExpired
		}
		observation, observeErr := supervisor.ReconcileInspection(ctx, identity)
		if shouldStopInspectionPolling(observeErr) {
			// ReconcileInspection has handed ownership of a still-running
			// supervisor command to Pending. Join it before returning and avoid
			// starting another status command after the admission context ends.
			joinInspectionPending(inFlightInspectionError(observeErr))
			return nil, observeErr
		}
		if observeErr == nil && observation.Matched {
			if err = operation.RecordReceiptContext(ctx, observation.Job.ID); err != nil {
				return nil, err
			}
			id := observation.Job.ID
			identity.NumericTaskID = &id
			if observation.Job.State == pueue.StateEnded {
				return completedInspectionFacts(ctx, operation, observation.Job)
			}
		}
		if err = waitInspectionObservation(ctx); err != nil {
			return nil, err
		}
	}
}

func shouldStopInspectionPolling(err error) bool {
	return inFlightInspectionError(err) != nil
}

func inFlightInspectionError(err error) *pueue.Pending {
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil {
		return nil
	}
	return inFlight.Pending
}

func joinInspectionPending(pending *pueue.Pending) {
	if pending == nil {
		return
	}
	<-pending.Done()
}

func completedInspectionFacts(ctx context.Context, operation *inspection.Operation, job pueue.Job) (json.RawMessage, error) {
	if !job.Succeeded {
		return nil, ErrProfileUnavailable
	}
	if err := operation.RecordWorkerSuccessContext(ctx, job.ID); err != nil {
		return nil, err
	}
	return operation.ReadProofContext(ctx)
}

func waitInspectionObservation(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return inspection.ErrAdmissionExpired
	case <-timer.C:
		return nil
	}
}
