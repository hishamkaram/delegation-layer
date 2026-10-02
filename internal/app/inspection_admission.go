package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	RunnerExecutable   string
	RunnerOwnership    string
	InspectionDeadline time.Time
	Capability         CapabilityReport
}

// supervisorOptionsForCandidate carries the provider's bounded, nonsecret
// launch environment into the ordinary supervisor client. The inspection
// worker is started by that supervisor and reconstructs the candidate from its
// process environment; using the same values here keeps its immutable
// definition identical to admission. Inspection commands receive a separate
// timeout-scoped client so ordinary supervisor controls keep their normal
// finite observation boundary.
func supervisorOptionsForCandidate(base pueue.Options, candidate commonprovider.ProfileCandidate) (pueue.Options, error) {
	if base.ObservationTimeout < 0 {
		return pueue.Options{}, pueue.ErrConfiguration
	}
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

func supervisorOptionsForMeta(base pueue.Options, meta task.MetaRecord) pueue.Options {
	return supervisorOptionsWithEnvironment(base, meta.Environment)
}

// supervisorOptionsForCurrentEnvironment snapshots the bounded supervisor
// control environment before runner reconstruction applies a task's provider
// environment to the process. This preserves Linux XDG socket/configuration
// selectors for external supervisors while keeping task credentials out of
// the supervisor context.
func supervisorOptionsForCurrentEnvironment(base pueue.Options) pueue.Options {
	if base.Environment != nil {
		base.Environment = append([]string(nil), base.Environment...)
		return base
	}
	base.Environment = pueue.DefaultEnvironment()
	return base
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
		// Provider profiles may isolate their own configuration with
		// XDG_CONFIG_HOME. Supervisor commands must retain the caller's
		// control configuration so their saved endpoint and identity remain
		// resolvable.
		if key == "XDG_CONFIG_HOME" {
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
		return admissionPreparation{}, withDispatchStage("prepare-provider", err)
	}
	profile, err := initialAdmissionProfile(candidate, req)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("finalize-provider", err)
	}
	runnerOwnership := requestedRunnerOwnership(a)
	a.runnerOwnership = runnerOwnership
	runner, err := prepareAdmissionExecutables(a, deps, store.Root)
	if err != nil {
		return admissionPreparation{}, err
	}
	a.Runner = runner
	supervisorOptions, err := supervisorOptionsForCandidate(deps.SupervisorOptions, candidate)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("prepare-supervisor-options", err)
	}
	supervisor, err := bindInitialWithOptions(a, deps, store.Root, supervisorOptions)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("start-supervisor", err)
	}
	if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		queueRepairRunner, runnerErr := admissionQueueRepairRunner(a, deps, runner)
		if runnerErr != nil {
			return admissionPreparation{}, withDispatchStage("start-supervisor", runnerErr)
		}
		if queueRepairRunner != "" {
			repairContext, cancelRepair := context.WithTimeout(context.Background(), pueue.RunnerCommandUpgradeTimeout)
			repairErr := repairQueuedRunnerCommands(repairContext, store, supervisor, queueRepairRunner)
			cancelRepair()
			if repairErr != nil {
				return admissionPreparation{}, withDispatchStage("start-supervisor", fmt.Errorf("repair queued task runner paths: %w", repairErr))
			}
		}
	}
	prepared := admissionPreparation{Profile: profile, Supervisor: supervisor, RunnerExecutable: runner, RunnerOwnership: runnerOwnership}
	if candidate.Inspection != nil {
		prepared.Profile, prepared.InspectionDeadline, candidate, err = finalizeAdmissionInspection(a, deps, store, req, candidate, supervisor)
		if err != nil {
			return admissionPreparation{}, err
		}
	}
	prepared.Capability, err = admissionRuntimeCapability(deps, req, candidate, prepared.Profile)
	if err != nil {
		return admissionPreparation{}, err
	}
	if err = prepared.Profile.ValidateStatePlacement(store.Root); err != nil {
		return admissionPreparation{}, withDispatchStage("validate-state-placement", err)
	}
	return prepared, nil
}

func initialAdmissionProfile(candidate commonprovider.ProfileCandidate, req task.TaskRecord) (PreparedProfile, error) {
	if candidate.Inspection != nil {
		return PreparedProfile{}, nil
	}
	return finalizeCandidate(candidate, req, nil)
}

func prepareAdmissionExecutables(a Arguments, deps Dependencies, root string) (string, error) {
	runnerOwnership := requestedRunnerOwnership(a)
	privateSupervisor, err := admissionUsesPrivateSupervisor(a, root)
	if err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	runnerArgument := a.Runner
	if runnerOwnership == task.RunnerOwnershipManaged {
		runnerArgument = ""
	} else if runnerArgument == "" {
		return "", withDispatchStage("resolve-runner", task.ErrEvidenceFault)
	}
	runner, err := resolveRunner(runnerArgument, deps)
	if err != nil {
		return "", withDispatchStage("resolve-runner", err)
	}
	if !privateSupervisor {
		return runner, nil
	}
	if runnerOwnership == task.RunnerOwnershipManaged {
		runner, err = installStateRunner(root, runner)
		if err != nil {
			return "", withDispatchStage("prepare-runner", err)
		}
	}
	if a.savedSupervisor != nil && privateSupervisorExecutablesAvailable(a.savedSupervisor.ClientExecutable, a.savedSupervisor.DaemonExecutable) {
		if err = installStateSupervisorPair(context.Background(), root, a.savedSupervisor.ClientExecutable, a.savedSupervisor.DaemonExecutable); err != nil {
			return "", withDispatchStage("prepare-supervisor-executables", err)
		}
		return runner, nil
	}
	clientExecutable, daemonExecutable, err := resolvePrivateSupervisorExecutables(root, deps)
	if err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	if err = installStateSupervisorPair(context.Background(), root, clientExecutable, daemonExecutable); err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	return runner, nil
}

func requestedRunnerOwnership(a Arguments) string {
	if a.runnerOwnership != "" {
		return a.runnerOwnership
	}
	if a.Runner != "" {
		return task.RunnerOwnershipCustom
	}
	return task.RunnerOwnershipManaged
}

func admissionQueueRepairRunner(a Arguments, deps Dependencies, taskRunner string) (string, error) {
	if requestedRunnerOwnership(a) == task.RunnerOwnershipManaged {
		return taskRunner, nil
	}
	runner, err := resolveRunner("", deps)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runner, nil
}

func admissionUsesPrivateSupervisor(a Arguments, root string) (bool, error) {
	if a.savedSupervisor != nil {
		return pueue.IsPrivateConfig(root, a.savedSupervisor.ConfigPath), nil
	}
	configPath, err := resolveInitialConfig(a)
	if err != nil {
		return false, err
	}
	if configPath != "" && pueue.IsPrivateConfig(root, configPath) {
		return false, fmt.Errorf("%w: explicit supervisor config collides with the private state-rooted supervisor", pueue.ErrConfiguration)
	}
	return configPath == "", nil
}

func finalizeAdmissionInspection(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (PreparedProfile, time.Time, commonprovider.ProfileCandidate, error) {
	var err error
	candidate, err = refreshAdmissionCandidate(deps, store.Root, req, candidate)
	if err != nil {
		return PreparedProfile{}, time.Time{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	facts, deadline, err := admissionInspectionFacts(a, deps, store, req, candidate, supervisor)
	if err != nil {
		return PreparedProfile{}, time.Time{}, commonprovider.ProfileCandidate{}, withDispatchStage("inspect-provider", err)
	}
	// The inspection worker reconstructs the candidate when it actually runs.
	// Refresh once more after its proof is complete so finalization uses the
	// same current provider identity that the worker inspected. The finalizer
	// still binds the returned facts to this candidate; a replacement during
	// this last handoff fails closed instead of relabeling the proof.
	candidate, err = refreshAdmissionCandidate(deps, store.Root, req, candidate)
	if err != nil {
		return PreparedProfile{}, time.Time{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return PreparedProfile{}, time.Time{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	return profile, deadline, candidate, nil
}

func admissionRuntimeCapability(deps Dependencies, req task.TaskRecord, candidate commonprovider.ProfileCandidate, profile PreparedProfile) (CapabilityReport, error) {
	registration, err := deps.normalized().Catalog.Lookup(req.Provider)
	if err != nil {
		return CapabilityReport{}, withDispatchStage("select-provider", err)
	}
	capability, err := runtimeCapability(req.Provider, candidate, profile, registration.Description)
	if err != nil {
		return CapabilityReport{}, withDispatchStage("record-runtime-capability", err)
	}
	return capability, nil
}

func ensureInspectionGroup(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (resultErr error) {
	group, err := inspection.OpenGroup(store, supervisor.Binding())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	capabilitySHA := ""
	if runtimeOnlyInspectionDefinition(definition) {
		capabilitySHA, err = commonprovider.RuntimeCapabilityDigest(definition)
		if err != nil {
			return nil, time.Time{}, annotateMissingInspectionStage("snapshotting capability", err)
		}
	}
	inspectionTimeout := inspection.TimeoutForRevision(definition.Revision)
	inspectionSupervisor, err := supervisor.WithObservationTimeout(inspectionTimeout)
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("preparing inspection supervisor", err)
	}
	if err = ensureInspectionGroup(store, inspectionSupervisor, inspectionTimeout); err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("ensuring inspection group", err)
	}
	binding := inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA, CapabilitySHA256: capabilitySHA,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: runner, WorkerSHA256: workerSHA,
		RunnerOwnership: requestedRunnerOwnership(a),
		Environment:     append([]string(nil), definition.Environment...), EnvironmentRecorded: true,
		Supervisor: supervisor.Binding(),
	}
	operation, err := openAdmissionInspectionOperation(store, req, binding, time.Now())
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("opening inspection operation", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Expired(time.Now()) {
		return nil, operation.Deadline(), inspection.ErrAdmissionExpired
	}
	ctx, cancel := context.WithDeadline(context.Background(), operation.Deadline())
	defer cancel()
	if err = submitInspectionOnce(ctx, operation, inspectionSupervisor, runner, store.Root); err != nil {
		return nil, operation.Deadline(), annotateMissingInspectionStage("submitting inspection", err)
	}
	facts, err = awaitInspection(ctx, operation, inspectionSupervisor)
	if err != nil {
		return nil, operation.Deadline(), annotateMissingInspectionStage("awaiting inspection", err)
	}
	if operation.Expired(time.Now()) {
		return nil, operation.Deadline(), inspection.ErrAdmissionExpired
	}
	return facts, operation.Deadline(), nil
}

func openAdmissionInspectionOperation(store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (operation *inspection.Operation, resultErr error) {
	operation, err := inspection.LoadOperation(store, request.TaskID)
	if errors.Is(err, os.ErrNotExist) {
		return inspection.OpenOperation(store, request, binding, now)
	}
	if err != nil {
		return nil, err
	}
	keepOperation := false
	defer func() {
		if !keepOperation {
			resultErr = errors.Join(resultErr, operation.Close())
		}
	}()

	record := operation.Request()
	requestBytes, err := task.MarshalCanonical(request)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	if record.RootID != store.RootID || record.TaskID != request.TaskID || record.TaskSHA256 != task.ComputeSHA256(requestBytes) {
		return nil, task.ErrEvidenceFault
	}
	if err = rejectCompletedRuntimeIdentityDrift(operation, record.Binding, binding); err != nil {
		return nil, err
	}
	if allowIncompleteMarkerlessRuntimeReplay(operation, record.Binding, binding) {
		// Keep the original create-once request. The worker will validate the
		// current runtime definition and establish its capability contract in
		// fresh supervised facts before this operation can become eligible.
		keepOperation = true
		return operation, nil
	}
	if sameInspectionBinding(record.Binding, binding) {
		keepOperation = true
		return operation, nil
	}
	if !sameInspectionBindingExceptManagedWorker(record.Binding, binding) {
		return nil, task.ErrEvidenceFault
	}
	if record.Binding.WorkerExecutable != binding.WorkerExecutable || record.Binding.WorkerSHA256 != binding.WorkerSHA256 {
		if err = validateManagedInspectionReplay(store.Root, operation, binding); err != nil {
			return nil, err
		}
	}
	keepOperation = true
	return operation, nil
}

// allowIncompleteMarkerlessRuntimeReplay lets a pre-capability-marker runtime
// journal survive a provider or probe-contract refresh while it is still
// incomplete. Its immutable worker, supervisor, and environment bindings must
// remain unchanged; only the runtime identity/definition may be re-probed.
// Completed markerless journals remain subject to the conservative identity
// drift rejection above because their proof cannot be relabeled in place.
func allowIncompleteMarkerlessRuntimeReplay(operation *inspection.Operation, saved, current inspection.Binding) bool {
	if operation == nil || saved.DefinitionRevision != commonprovider.RuntimeInspectionRevision || current.DefinitionRevision != commonprovider.RuntimeInspectionRevision || saved.CapabilitySHA256 != "" || current.CapabilitySHA256 == "" || (saved.RunnerOwnership != "" && saved.RunnerOwnership != current.RunnerOwnership) || saved.WorkerExecutable != current.WorkerExecutable || saved.WorkerSHA256 != current.WorkerSHA256 || !task.SameSupervisorIdentity(saved.Supervisor, current.Supervisor) {
		return false
	}
	savedEnvironmentRecorded := saved.EnvironmentRecorded || len(saved.Environment) > 0
	currentEnvironmentRecorded := current.EnvironmentRecorded || len(current.Environment) > 0
	if savedEnvironmentRecorded != currentEnvironmentRecorded || !slices.Equal(saved.Environment, current.Environment) {
		return false
	}
	_, _, err := operation.ReadCompletedContext(context.Background())
	return errors.Is(err, os.ErrNotExist)
}

// rejectCompletedRuntimeIdentityDrift prevents a completed capability proof
// from being relabeled as proof for a different provider executable. A queued
// or incomplete operation can still run its one worker against the current
// candidate; a completed operation has no safe way to rerun that worker under
// the same create-once journal.
func rejectCompletedRuntimeIdentityDrift(operation *inspection.Operation, saved, current inspection.Binding) error {
	if operation == nil ||
		(saved.CapabilitySHA256 == "" && current.CapabilitySHA256 == "") ||
		saved.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		current.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		saved.DefinitionSHA256 == current.DefinitionSHA256 {
		return nil
	}
	if _, _, err := operation.ReadCompletedContext(context.Background()); err == nil {
		return task.ErrEvidenceFault
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}

func validateManagedInspectionReplay(root string, operation *inspection.Operation, binding inspection.Binding) error {
	if !managedInspectionWorkerExecutable(root, binding) {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(context.Background())
	if err != nil {
		return err
	}
	if authorized {
		return nil
	}
	// A retry may reopen a completed operation after its queued worker is gone.
	// The later supervisor reconciliation still verifies the matching
	// successful job before these facts can admit the request.
	result, _, err := operation.ReadCompletedContext(context.Background())
	if err != nil || result.Reason != inspection.ResultEligible {
		return task.ErrEvidenceFault
	}
	return nil
}

func sameInspectionBinding(left, right inspection.Binding) bool {
	return sameInspectionBindingBase(left, right) &&
		(left.RunnerOwnership == right.RunnerOwnership || left.RunnerOwnership == "") &&
		left.WorkerExecutable == right.WorkerExecutable &&
		left.WorkerSHA256 == right.WorkerSHA256
}

func sameInspectionBindingBase(left, right inspection.Binding) bool {
	return sameInspectionEnvironment(left, right) &&
		task.SameSupervisorIdentity(left.Supervisor, right.Supervisor) &&
		sameInspectionDefinition(left, right)
}

func sameInspectionEnvironment(left, right inspection.Binding) bool {
	leftEnvironmentRecorded := left.EnvironmentRecorded || len(left.Environment) > 0
	rightEnvironmentRecorded := right.EnvironmentRecorded || len(right.Environment) > 0
	return leftEnvironmentRecorded == rightEnvironmentRecorded && slices.Equal(left.Environment, right.Environment)
}

func sameInspectionDefinition(left, right inspection.Binding) bool {
	if left.DefinitionRevision == commonprovider.RuntimeInspectionRevision && right.DefinitionRevision == commonprovider.RuntimeInspectionRevision {
		return sameRuntimeInspectionDefinition(left, right)
	}
	return sameExactInspectionDefinition(left, right)
}

func sameRuntimeInspectionDefinition(left, right inspection.Binding) bool {
	// Only bindings carrying the runtime-only capability contract may relax
	// executable identity. A mixed native/runtime binding without that marker
	// remains exact so native command and helper changes cannot be silently
	// reused.
	if left.CapabilitySHA256 != "" && right.CapabilitySHA256 != "" {
		return left.CapabilitySHA256 == right.CapabilitySHA256
	}
	return sameExactInspectionDefinition(left, right)
}

func sameExactInspectionDefinition(left, right inspection.Binding) bool {
	return left.DefinitionRevision == right.DefinitionRevision &&
		left.DefinitionSHA256 == right.DefinitionSHA256 &&
		left.HelperExecutable == right.HelperExecutable &&
		left.HelperSHA256 == right.HelperSHA256
}

func sameInspectionBindingExceptManagedWorker(saved, current inspection.Binding) bool {
	return saved.RunnerOwnership == task.RunnerOwnershipManaged &&
		current.RunnerOwnership == task.RunnerOwnershipManaged &&
		sameInspectionBindingBase(saved, current)
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
		observation, observeErr := reconcileInspectionObservation(ctx, operation, supervisor, identity)
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
				// A worker that reaches its own deadline can be observed as a
				// failed terminal job at the same boundary. The persisted
				// admission deadline is authoritative: do not turn that boundary
				// into a provider rejection or record worker success after it.
				if operation.Expired(time.Now()) || ctx.Err() != nil {
					return nil, inspection.ErrAdmissionExpired
				}
				return completedInspectionFacts(ctx, operation, observation.Job)
			}
		}
		if err = waitInspectionObservation(ctx); err != nil {
			return nil, err
		}
	}
}

func reconcileInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity) (pueue.InspectionObservation, error) {
	if inspectionDeadlineReached(ctx, operation) {
		return pueue.InspectionObservation{}, inspection.ErrAdmissionExpired
	}
	observation, observeErr := supervisor.ReconcileInspection(ctx, identity)
	if inspectionDeadlineReached(ctx, operation) {
		// The supervisor observation may reach the deadline with either an
		// in-flight status command or a terminal failed worker. The persisted
		// admission deadline is authoritative in both cases; retain command
		// ownership before returning the stable expiry result.
		joinInspectionPending(inFlightInspectionError(observeErr))
		return observation, inspection.ErrAdmissionExpired
	}
	return observation, observeErr
}

func inspectionDeadlineReached(ctx context.Context, operation *inspection.Operation) bool {
	return ctx.Err() != nil || operation.Expired(time.Now())
}

func shouldStopInspectionPolling(err error) bool {
	return errors.Is(err, inspection.ErrAdmissionExpired) || inFlightInspectionError(err) != nil
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
