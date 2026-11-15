package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

var errSupervisorCleanup = errors.New("supervisor migration client cleanup failed")

func authorizePrivateSuccessfulCleanup(ctx context.Context, stateRoot string, supervisor task.SupervisorRef, jobs []pueue.Job) (plan pueue.PrivateQueueCleanupPlan, resultErr error) {
	return privateCleanupOwner().AuthorizeSuccessfulCleanup(ctx, stateRoot, supervisor, jobs)
}

// authorizePrivateMigrationQueue is the application-owned migration policy.
// Pueue supplies the snapshot and performs only the already-authorized queue
// transition; all Delegate labels and terminal evidence stay here.
func authorizePrivateMigrationQueue(ctx context.Context, stateRoot string, legacy task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	var store *taskdir.Store
	if err := openPrivateMigrationStore(ctx, stateRoot, &store); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	_, err := privateCleanupOwner().InspectMigrationQueue(ctx, store, supervisorcore.InspectionGroup(store.RootID, legacy), jobs)
	if err != nil && !supervisorcore.MaintenanceBusy(err) {
		return errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)
	}
	return err
}

func openPrivateMigrationStore(ctx context.Context, stateRoot string, store **taskdir.Store) error {
	if *store != nil {
		return nil
	}
	var err error
	*store, err = taskdir.OpenStoreWithPredicatesContext(ctx, stateRoot, predicate.Default())
	if err != nil {
		return fmt.Errorf("%w: inspect ended Delegate task state: %w", pueue.ErrPrivateSupervisorMigrationRequired, err)
	}
	return nil
}

func supervisorCommand(a Arguments, deps Dependencies) (result commandResult) {
	if a.SupervisorAction == "restart" {
		return supervisorRuntimeRestart(a, deps)
	}
	if a.SupervisorAction == "status" && a.SupervisorRuntimeRestart {
		return supervisorRuntimeRestartPreflight(a, deps)
	}
	if a.SupervisorAction == "ensure-owner" {
		return supervisorEnsureOwner(a, deps)
	}
	if a.SupervisorAction == "stop-owner" {
		return supervisorOwnerStop(a, deps)
	}
	response := newSupervisorCommandResponse(a.SupervisorAction)
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failedSupervisor(response, "supervisor state root is unavailable", 1)
	}
	if a.SupervisorAction == "status" {
		return supervisorStatus(root, deps, response)
	}
	return supervisorMigrate(root, deps, response)
}

// Taskdir owns root validity. Missing required metadata cannot make an
// existing root namespace an uninitialized supervisor installation.
func supervisorRootAbsent(root string, operationErr error) bool {
	if !task.RecordAbsent(operationErr) {
		return false
	}
	_, namespaceErr := os.Lstat(root)
	return task.RecordAbsent(namespaceErr)
}

func supervisorStatus(root string, deps Dependencies, response Response) (result commandResult) {
	defer func() { observeSupervisorInstallation(root, deps, &result) }()
	options := supervisorControlOptions(deps)
	controlContext, cancel := supervisorcore.ControlContext(options)
	defer cancel()
	store, err := openStoreContext(controlContext, root, deps.storeDependencies(), false)
	if supervisorRootAbsent(root, err) {
		return notInitializedSupervisor(response)
	}
	if err != nil {
		return failedSupervisor(response, "supervisor state root is unavailable", 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	result = supervisorStatusFromStore(controlContext, root, deps, options, response, store)
	applyMaintenanceGuidance(result.response.SupervisorControl)
	return result
}

func observeSupervisorInstallation(root string, deps Dependencies, result *commandResult) {
	control := result.response.SupervisorControl
	if control == nil {
		return
	}
	binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
	var recorded *task.SupervisorRef
	if found && err == nil {
		recorded = &binding
	}
	observation := installation.ObserveSupervisorBinding(installation.BindingRuntimeSelected, installation.ObserveSelectedSupervisorPair(deps.supervisorLookup()), recorded, err)
	control.InstallationBinding = &observation
	if result.code == 0 && control.FirstBlocker == nil && control.NextAction == "none" && observation.HandoffAdvised() {
		control.NextAction = "run_supervisor_ensure_owner"
	}
}

func supervisorStatusFromStore(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response, store *taskdir.Store) commandResult {
	controlContext := ctx
	seed, _, err := inspection.ReadGroupHead(controlContext, store)
	if errors.Is(err, os.ErrNotExist) {
		return supervisorStatusWithoutInspectionGroup(controlContext, root, deps, options, response, store)
	}
	if err != nil {
		return failedSupervisorCause(response, "supervisor migration journal could not be read", err, 1)
	}
	if !pueue.IsPrivateConfig(root, seed.Supervisor.ConfigPath) {
		response.SupervisorControl.State = "external"
		response.SupervisorControl.LegacyReachability = "not_managed"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		return commandResult{response: response, code: 0}
	}
	resolution, err := supervisorcore.ResolveGeneration(controlContext, store, seed.Supervisor)
	if err != nil {
		if seed.Supervisor.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
			response.SupervisorControl.NextAction = "stop_and_report"
		}
		if seed.Supervisor.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 && errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
			resumePending, phaseErr := privateSupervisorMigrationResumePending(ctx, root)
			if phaseErr != nil {
				return failedSupervisorCause(response, "supervisor migration journal could not be resolved", errors.Join(err, phaseErr), 1)
			}
			if resumePending {
				response.SupervisorControl.NextAction = "run_supervisor_migrate"
			}
		}
		return failedSupervisorCause(response, "supervisor migration journal could not be resolved", err, 1)
	}
	active := resolution.GroupHead
	if !resolution.GroupPresent || active.Supervisor != resolution.Active {
		active = supervisorcore.InspectionGroup(store.RootID, resolution.Active)
	}
	migrated := resolution.GroupMigrated || (resolution.PrivateAuthority && seed.Supervisor != resolution.Active)
	return supervisorLegacyStatus(controlContext, root, deps, options, response, active, migrated, store)
}

func supervisorStatusWithoutInspectionGroup(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response, store *taskdir.Store) (result commandResult) {
	if result, done := privateSupervisorConfigStatus(root, response); done {
		return result
	}

	currentOwner, currentErr := supervisorcore.AcquireClient(currentPrivateSupervisor(ctx, root, deps, options))
	cleanupCause := currentErr
	defer func() { supervisorcore.CloseOwnerMaintenance(currentOwner, cleanupCause) }()
	if currentErr != nil {
		response.SupervisorControl.State = supervisorStatusState(true, false)
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		return commandResult{response: response, code: 1, err: errors.New("private supervisor could not be verified")}
	}
	current := currentOwner.Client()
	logicalDrift, err := pueue.PrivateSupervisorLogicalDrift(root, current.Binding())
	if err != nil {
		return failedSupervisor(response, "private supervisor identity could not be inspected", 1)
	}
	ready, readyErr := current.Reachable(ctx)
	cleanupCause = readyErr
	if readyErr == nil && !ready {
		saved, found, savedErr := pueue.ReadPrivateSupervisorBinding(root)
		if savedErr != nil {
			return failedSupervisorCause(response, "private supervisor identity could not be inspected", savedErr, 1)
		}
		if found && saved.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
			response.SupervisorControl.State = "legacy_present"
			response.SupervisorControl.NextAction = "run_supervisor_migrate"
			return legacySupervisorWorkStatus(ctx, root, options, response, saved, current.Binding().ClientExecutable)
		}
	}
	if readyErr != nil || !ready {
		return privateStatusUnavailable(response, logicalDrift, false, readyErr)
	}
	response.SupervisorControl.State = supervisorStatusState(logicalDrift, true)
	response.SupervisorControl.LegacyReachability = "ready"
	snapshot, snapshotErr := current.Snapshot(ctx)
	if snapshotErr != nil {
		cleanupCause = snapshotErr
		return failedSupervisor(response, "private supervisor status could not be read", 1)
	}
	if err := observeSupervisorMaintenance(ctx, store, current.Binding(), snapshot.Jobs, response.SupervisorControl); err != nil {
		return failedSupervisorCause(response, "private supervisor maintenance could not be inspected", err, 1)
	}
	if !logicalDrift {
		if result, done := supervisorAdmissionStatus(ctx, store, deps, options, response, false); done {
			return result
		}
	}
	response.SupervisorControl.NextAction = readySupervisorNextAction(logicalDrift, response.SupervisorControl.ActiveWork)
	return commandResult{response: response, code: 0}
}

// A ready endpoint needs migration only for logical drift. Active legacy work
// must finish before the operator can take that explicit transition.
func readySupervisorNextAction(drift bool, activeWork string) string {
	if !drift {
		return "none"
	}
	if activeWork == "present" {
		return "wait_for_legacy_work"
	}
	return "run_supervisor_migrate"
}

// Config absence and read faults are distinct from live admission readiness.
func privateSupervisorConfigStatus(root string, response Response) (commandResult, bool) {
	_, err := os.Stat(pueue.PrivateConfigPath(root))
	switch {
	case err == nil:
		return commandResult{}, false
	case errors.Is(err, os.ErrNotExist):
		return notInitializedSupervisor(response), true
	default:
		return failedSupervisor(response, "private supervisor state could not be inspected", 1), true
	}
}

// privateStatusUnavailable presents a failed or negative readiness probe.
// Status only observes a refusal; dispatch or migrate owns its closure.
func privateStatusUnavailable(response Response, drift, historical bool, readyErr error) commandResult {
	response.SupervisorControl.State = supervisorStatusState(drift, false)
	response.SupervisorControl.LegacyReachability = "unreachable"
	response.SupervisorControl.ActiveWork = "unknown"
	response.SupervisorControl.NextAction = "run_supervisor_migrate"
	switch {
	case readyErr == nil && historical:
		response.SupervisorControl.ReasonCode = "historical_target_not_ready"
		response.SupervisorControl.NextAction = "stop_and_report"
		cause := &pueue.PrivateRecoveryError{
			ReasonCode: "historical_target_not_ready",
			Err:        errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, pueue.ErrHistoricalTargetNotReady),
		}
		return commandResult{response: response, code: 1, err: publicSupervisorError("The historical supervisor is unreachable.", cause)}
	case errors.Is(readyErr, pueue.ErrPrivateEndpointRefused):
		return commandResult{response: response, code: 1, err: &supervisorPublicError{message: "private supervisor is unreachable", cause: readyErr}}
	case readyErr != nil:
		response.SupervisorControl.LegacyReachability = "incompatible"
		return commandResult{response: response, code: 1, err: errors.New("private supervisor could not be verified")}
	default:
		return commandResult{response: response, code: 1, err: errors.New("private supervisor is unreachable")}
	}
}

func supervisorStatusState(drift, ready bool) string {
	if drift {
		return "legacy_present"
	}
	if ready {
		return "ready"
	}
	return "unavailable"
}

func supervisorLegacyStatus(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response, legacy inspection.GroupRequestRecord, migrated bool, store *taskdir.Store) (result commandResult) {
	currentOwner, currentErr := supervisorcore.AcquireClient(currentPrivateSupervisor(ctx, root, deps, options))
	cleanupCause := currentErr
	defer func() { supervisorcore.CloseOwnerMaintenance(currentOwner, cleanupCause) }()
	current := currentOwner.Client()
	if currentErr != nil && current != nil {
		response.SupervisorControl.State = "unavailable"
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		return commandResult{
			response: response,
			code:     1,
			err:      errors.Join(errors.New("private supervisor could not be verified"), currentErr),
		}
	}
	currentMatches := currentErr == nil && task.SameSupervisorIdentity(current.Binding(), legacy.Supervisor)
	currentReady := false
	if currentMatches {
		currentReady, currentErr = current.Reachable(ctx)
		cleanupCause = currentErr
	}
	if currentMatches && currentReady {
		snapshot, snapshotErr := current.Snapshot(ctx)
		if snapshotErr != nil {
			cleanupCause = snapshotErr
			return failedSupervisor(response, "private supervisor status could not be read", 1)
		}
		if err := observeSupervisorMaintenance(ctx, store, legacy.Supervisor, snapshot.Jobs, response.SupervisorControl); err != nil {
			return failedSupervisorCause(response, "private supervisor maintenance could not be inspected", err, 1)
		}
		if result, done := supervisorAdmissionStatus(ctx, store, deps, options, response, migrated); done {
			return result
		}
	}
	if result, done := legacySupervisorStateResult(response, legacy, migrated, currentMatches, currentReady, currentErr); done {
		return result
	}
	if currentMatches {
		if legacy.Supervisor.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
			return legacySupervisorWorkStatus(ctx, root, options, response, legacy.Supervisor, current.Binding().ClientExecutable)
		}
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor requires managed migration")}
	}
	fallbackClient := ""
	if current != nil {
		fallbackClient = current.Binding().ClientExecutable
	}
	return legacySupervisorWorkStatus(ctx, root, options, response, legacy.Supervisor, fallbackClient)
}

// Migration completion is history. Current scheduling readiness is derived
// passively from the canonical generation and selected installed transport.
func supervisorAdmissionStatus(ctx context.Context, store *taskdir.Store, deps Dependencies, options pueue.Options, response Response, migrated bool) (commandResult, bool) {
	if migrated {
		response.SupervisorControl.MigrationPhase = "complete"
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil {
		return failedSupervisorCause(response, "private supervisor identity could not be inspected", err, 1), true
	}
	// Historical generations retain their observation and explicit-migration
	// policy. Fresh installed-generation admission readiness is a separate fact.
	if !found || published.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
		return commandResult{}, false
	}
	readiness, err := (supervisorcore.ControlRuntime{Options: options, Lookup: deps.supervisorLookup()}).ObserveAdmission(ctx, store)
	if err != nil {
		response.SupervisorControl.NextAction = "stop_and_report"
		if errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
			response.SupervisorControl.NextAction = "run_supervisor_migrate"
		}
		return failedSupervisorCause(response, "private supervisor admission readiness could not be verified", err, 1), true
	}
	if readiness == supervisorcore.AdmissionReady {
		return commandResult{}, false
	}
	response.SupervisorControl.State = "unavailable"
	response.SupervisorControl.LegacyReachability = "ready"
	if readiness == supervisorcore.AdmissionSchedulingRefreshRequired {
		response.SupervisorControl.ReasonCode = string(infrastructure.PreparationReasonSchedulingRefreshRequired)
	}
	response.SupervisorControl.NextAction = "run_supervisor_ensure_owner"
	return failedSupervisor(response, "Supervisor admission is not ready; prepare its owner to verify scheduling evidence.", 1), true
}

func legacySupervisorStateResult(response Response, legacy inspection.GroupRequestRecord, migrated, currentMatches, currentReady bool, currentErr error) (commandResult, bool) {
	if currentMatches && currentErr != nil {
		response.SupervisorControl.State = "unavailable"
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		err := errors.New("private supervisor could not be verified")
		if errors.Is(currentErr, pueue.ErrPrivateEndpointRefused) {
			response.SupervisorControl.LegacyReachability = "unreachable"
			err = &supervisorPublicError{message: "private supervisor is unreachable", cause: currentErr}
		}
		return commandResult{response: response, code: 1, err: err}, true
	}
	if migrated && currentMatches && currentReady {
		response.SupervisorControl.State = "migrated"
		response.SupervisorControl.LegacyReachability = "completed"
		response.SupervisorControl.MigrationPhase = "complete"
		response.SupervisorControl.NextAction = "none"
		return commandResult{response: response, code: 0}, true
	}
	if currentReady {
		response.SupervisorControl.State = "ready"
		response.SupervisorControl.LegacyReachability = "ready"
		response.SupervisorControl.NextAction = "none"
		if migrated {
			response.SupervisorControl.State = "migrated"
			response.SupervisorControl.LegacyReachability = "completed"
			response.SupervisorControl.MigrationPhase = "complete"
			return commandResult{response: response, code: 0}, true
		}
		return commandResult{response: response, code: 0}, true
	} else {
		response.SupervisorControl.State = "legacy_present"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
	}
	return commandResult{}, false
}

func legacySupervisorWorkStatus(ctx context.Context, root string, options pueue.Options, response Response, legacy task.SupervisorRef, fallbackClient string) (result commandResult) {
	clientOwner, err := supervisorcore.AcquireClient(pueue.BindSavedForMigrationWithClient(ctx, legacy, fallbackClient, options))
	cleanupCause := err
	defer func() {
		if cleanupErr := clientOwner.CloseAfterPending(cleanupCause); cleanupErr != nil {
			// Supervisor JSON is a stable control surface. Keep cleanup details,
			// including temporary config paths, out of it.
			result.err = &supervisorPublicError{message: errSupervisorCleanup.Error(), cause: errors.Join(errSupervisorCleanup, result.err, cleanupErr)}
			result.code = 1
			result.response.setError(result.err)
		}
	}()
	if err != nil {
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.Join(errors.New("legacy supervisor requires managed migration"), err)}
	}
	client := clientOwner.Client()
	ready, probeErr := client.Reachable(ctx)
	if probeErr != nil {
		cleanupCause = probeErr
		response.SupervisorControl.LegacyReachability = "incompatible"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor could not be verified")}
	}
	if !ready {
		if legacy.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
			resumePending, phaseErr := privateSupervisorMigrationResumePending(ctx, root)
			if phaseErr != nil {
				cleanupCause = phaseErr
				response.SupervisorControl.NextAction = "stop_and_report"
				return failedSupervisorCause(response, "private supervisor migration state could not be inspected", phaseErr, 1)
			}
			if !resumePending {
				return privateStatusUnavailable(response, true, true, nil)
			}
		}
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor requires managed migration")}
	}
	response.SupervisorControl.LegacyReachability = "ready"
	snapshot, snapshotErr := client.Snapshot(ctx)
	if snapshotErr != nil {
		cleanupCause = snapshotErr
		return failedSupervisor(response, "legacy supervisor status could not be read", 1)
	}
	if err := observeSupervisorMaintenanceAtRoot(ctx, root, legacy, snapshot.Jobs, response.SupervisorControl); err != nil {
		return failedSupervisorCause(response, "legacy supervisor maintenance could not be inspected", err, 1)
	}
	if response.SupervisorControl.ActiveWork == "none" {
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
	} else {
		response.SupervisorControl.NextAction = "wait_for_legacy_work"
	}
	return commandResult{response: response, code: 0}
}

// The migration owner retains all replay authority. Status only reads its
// canonical phase facts under a shared bootstrap lease that excludes writers.
func privateSupervisorMigrationResumePending(ctx context.Context, root string) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var resumePending bool
	err := pueue.WithSupervisorLeaseContext(ctx, root, func(context.Context) error {
		pending, pendingErr := pueue.PrivateSupervisorMigrationPending(root)
		if pendingErr != nil || !pending {
			return pendingErr
		}
		retargetable, readErr := pueue.PrivateMigrationRetargetableWithLock(root)
		resumePending = readErr == nil && !retargetable
		return readErr
	})
	return resumePending && err == nil, err
}

func supervisorControlOptions(deps Dependencies) pueue.Options {
	options := deps.supervisorControlScope()
	if options.ObservationTimeout == 0 {
		options.ObservationTimeout = pueue.DefaultObservationTimeout
	}
	return options
}

func currentPrivateSupervisor(ctx context.Context, root string, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	clientExecutable, _, err := installation.ResolvePrivateSupervisorExecutables(deps.supervisorLookup())
	if err != nil {
		return nil, err
	}
	return pueue.Bind(ctx, clientExecutable, pueue.PrivateConfigPath(root), options)
}

func prepareCurrentPrivateSupervisor(ctx context.Context, root string, lookup installation.ExecutableLookup, options pueue.Options) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := migrationSupervisorPair(lookup)
	if err != nil {
		return nil, err
	}
	return pueue.PreparePrivateForMigrationWithLock(ctx, clientExecutable, daemonExecutable, root, options)
}

type supervisorMigrationState struct {
	current                 *pueue.Client
	noInspectionGroup       bool
	notInitialized          bool
	externalInspectionGroup bool
	activeWork              string
}

func supervisorMigrate(root string, deps Dependencies, response Response) (result commandResult) {
	defer func() { observeSupervisorInstallation(root, deps, &result) }()
	options := supervisorControlOptions(deps)
	controlContext, cancel := supervisorcore.ControlContext(options)
	defer cancel()
	store, err := openStoreContext(controlContext, root, deps.storeDependencies(), false)
	if supervisorRootAbsent(root, err) {
		return notInitializedSupervisor(response)
	}
	if err != nil {
		return failedSupervisor(response, "supervisor state root could not be opened", 1)
	}
	defer func() { closeObservationStore(&result, store) }()
	response.RootID = store.RootID
	initialized, err := supervisorMigrationInitialized(controlContext, store)
	if err != nil {
		return supervisorMigrationFailure(response, err)
	}
	if !initialized {
		return notInitializedSupervisor(response)
	}
	runtime := supervisorcore.ControlRuntime{Options: options, Lookup: deps.supervisorLookup(), Routing: deps.infrastructureRouting()}
	_, report, err := runtime.MigrateOwn(controlContext, store)
	result = finishRoutedSupervisorMigration(response, report, err)
	if err != nil {
		diagnostic := supervisorMaintenanceDiagnostic(controlContext, store, deps, options)
		result = finishSupervisorDiagnostic(result, diagnostic, err)
	}
	return result
}

// This later snapshot observes only the saved queue and maintenance evidence.
// Admission readiness and installation preparation are independent contracts.
func supervisorMaintenanceDiagnostic(ctx context.Context, store *taskdir.Store, deps Dependencies, options pueue.Options) (result commandResult) {
	response := newSupervisorCommandResponse("status")
	control := response.SupervisorControl
	defer func() { control.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano) }()
	binding, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil {
		return failedSupervisorCause(response, "saved supervisor binding could not be inspected", err, 1)
	}
	if !found {
		return failedSupervisorCause(response, "saved supervisor binding is absent", pueue.ErrPrivateSupervisorNotInitialized, 1)
	}
	clientExecutable, _, err := installation.ResolvePrivateSupervisorExecutables(deps.supervisorLookup())
	if err != nil {
		return failedSupervisorCause(response, "installed supervisor transport could not be inspected", err, 1)
	}
	owner, err := supervisorcore.AcquireClient(pueue.BindSavedForMigrationWithClient(ctx, binding, clientExecutable, options))
	cleanupCause := err
	defer func() {
		if closeErr := owner.CloseAfterPending(cleanupCause); closeErr != nil {
			result.err = publicSupervisorError("supervisor diagnostic cleanup failed", errors.Join(result.err, closeErr))
			result.code = 1
			control.MigrationReadiness = "unknown"
			control.FirstBlocker = &supervisorcore.MaintenanceBlocker{Kind: "observation", ReasonCode: "observation_failed", NextAction: "stop_and_report"}
		}
	}()
	if err != nil {
		return failedSupervisorCause(response, "saved supervisor transport could not be inspected", err, 1)
	}
	snapshot, err := owner.Client().Snapshot(ctx)
	cleanupCause = err
	if err != nil {
		return failedSupervisorCause(response, "saved supervisor queue could not be inspected", err, 1)
	}
	if err = observeSupervisorMaintenance(ctx, store, binding, snapshot.Jobs, control); err != nil {
		return failedSupervisorCause(response, "saved supervisor maintenance could not be inspected", err, 1)
	}
	return commandResult{response: response}
}

func finishSupervisorDiagnostic(result, diagnostic commandResult, original error) commandResult {
	control := result.response.SupervisorControl
	attachSupervisorDiagnostic(control, diagnostic.response.SupervisorControl)
	if diagnostic.err != nil {
		result.err = publicSupervisorError("Supervisor migration and its later diagnostic could not be completed.", errors.Join(result.err, diagnostic.err))
		control.NextAction = "stop_and_report"
	} else if supervisorcore.MaintenanceBusy(original) {
		applyMaintenanceGuidance(control)
	}
	return result
}

func notInitializedSupervisor(response Response) commandResult {
	response.SupervisorControl.State = "not_initialized"
	response.SupervisorControl.LegacyReachability = "not_present"
	response.SupervisorControl.ActiveWork = "none"
	response.SupervisorControl.MigrationReadiness = "clear"
	response.SupervisorControl.NextAction = "none"
	return commandResult{response: response, code: 0}
}

func inspectSupervisorMigration(ctx context.Context, root string, lookup installation.ExecutableLookup, options pueue.Options, store *taskdir.Store) (state supervisorMigrationState, resultErr error) {
	state.activeWork = "none"
	initialized, err := supervisorMigrationInitialized(ctx, store)
	if err != nil {
		return state, err
	}
	if !initialized {
		state.notInitialized = true
		return state, nil
	}
	observationTimeout := options.ObservationTimeout
	if observationTimeout <= 0 {
		observationTimeout = pueue.DefaultObservationTimeout
	}
	resultErr = pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		return inspectSupervisorMigrationLocked(lockedContext, root, lookup, options, store, observationTimeout, &state)
	})
	return state, resultErr
}

// The command and fixed host share this observational preflight. Root and
// inspection journals remain authoritative; no lock or preparation crosses RPC.
func supervisorMigrationInitialized(ctx context.Context, store *taskdir.Store) (bool, error) {
	present, err := privateSupervisorStatePresent(store.Root)
	if err != nil {
		return false, fmt.Errorf("private supervisor state could not be inspected: %w", err)
	}
	if !present {
		_, _, groupErr := inspection.ReadGroupHead(ctx, store)
		if task.RecordAbsent(groupErr) {
			return false, nil
		}
		if groupErr != nil {
			return false, fmt.Errorf("inspection supervisor journal could not be inspected: %w", groupErr)
		}
	}
	return true, nil
}

func privateSupervisorStatePresent(root string) (bool, error) {
	configPath := pueue.PrivateConfigPath(root)
	if _, err := os.Stat(configPath); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(filepath.Dir(configPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	_, err := os.Stat(filepath.Dir(configPath))
	return err == nil, nil
}

func inspectSupervisorMigrationLocked(ctx context.Context, root string, lookup installation.ExecutableLookup, options pueue.Options, store *taskdir.Store, observationTimeout time.Duration, state *supervisorMigrationState) error {
	if err := supervisorcore.ReconcilePublishedSupervisorWithLock(ctx, store, options, lookup, true); err != nil {
		return err
	}
	legacy, _, journalErr := inspection.ReadGroupHead(ctx, store)
	if errors.Is(journalErr, os.ErrNotExist) {
		if err := inspectSupervisorWithoutGroup(ctx, root, lookup, options, state); err != nil || state.notInitialized {
			return err
		}
		return verifyCommittedPrivateSupervisor(root, state.current)
	}
	if journalErr != nil {
		return fmt.Errorf("supervisor migration journal could not be read: %w", journalErr)
	}
	state.externalInspectionGroup = !pueue.IsPrivateConfig(root, legacy.Supervisor.ConfigPath)
	var current *pueue.Client
	var err error
	if state.externalInspectionGroup {
		current, err = prepareInspectionHandoffSupervisor(ctx, root, lookup, options)
	} else {
		current, err = prepareInspectionMigrationSupervisorWithLock(ctx, store, lookup, options)
	}
	state.current = current
	if err != nil {
		return err
	}
	if state.externalInspectionGroup {
		if err = supervisorcore.MigrateInspectionGroup(ctx, store, current, observationTimeout, legacy); err != nil {
			return err
		}
	}
	if !state.externalInspectionGroup {
		if err = supervisorcore.MigrateInspectionSupervisorLocked(ctx, store, current, observationTimeout); err != nil {
			return err
		}
	}
	if err = supervisorcore.EnsureInspectionGroupLocked(ctx, store, current, observationTimeout, false); err != nil {
		return err
	}
	return verifyCommittedPrivateSupervisor(root, current)
}

func verifyCommittedPrivateSupervisor(root string, current *pueue.Client) error {
	active, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil {
		return err
	}
	if !found || active != current.Binding() {
		return &pueue.PrivateRecoveryError{ReasonCode: "transition_pending", Err: pueue.ErrPrivateSupervisorMigrationRequired}
	}
	return nil
}

func inspectSupervisorWithoutGroup(ctx context.Context, root string, lookup installation.ExecutableLookup, options pueue.Options, state *supervisorMigrationState) error {
	state.noInspectionGroup = true
	if _, err := os.Stat(pueue.PrivateConfigPath(root)); errors.Is(err, os.ErrNotExist) {
		state.notInitialized = true
		return nil
	} else if err != nil {
		return fmt.Errorf("private supervisor state could not be inspected: %w", err)
	}
	current, err := supervisorMigrateWithoutInspectionGroupLocked(ctx, root, lookup, options)
	if err != nil {
		state.current = current
		return err
	}
	state.current = current
	snapshot, err := current.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("private supervisor status could not be read: %w", err)
	}
	state.activeWork = legacyActiveWork(snapshot)
	return nil
}

func finishSupervisorMigration(response Response, state supervisorMigrationState, err error) commandResult {
	if err != nil {
		return supervisorMigrationFailure(response, err)
	}
	response.SupervisorControl.State = "migrated"
	if state.externalInspectionGroup {
		response.SupervisorControl.LegacyReachability = "not_managed"
		response.SupervisorControl.ActiveWork = "unknown"
	} else if state.noInspectionGroup {
		response.SupervisorControl.LegacyReachability = "not_present"
		response.SupervisorControl.ActiveWork = state.activeWork
	} else {
		response.SupervisorControl.LegacyReachability = "recovered"
		response.SupervisorControl.ActiveWork = "none"
	}
	response.SupervisorControl.MigrationPhase = "complete"
	response.SupervisorControl.NextAction = "retry_dispatch"
	return commandResult{response: response, code: 0}
}

func supervisorMigrationFailure(response Response, err error) commandResult {
	control := response.SupervisorControl
	control.ReasonCode = supervisorcore.FailureReason(err)
	control.MigrationReadiness = "unknown"
	control.ActiveWork = "unknown"
	var diagnostic *supervisorcore.MaintenanceError
	if errors.As(err, &diagnostic) {
		applyMaintenanceAssessment(control, diagnostic.Assessment)
		control.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if supervisorcore.MaintenanceBusy(err) && diagnostic != nil {
		control.State = "blocked"
		control.NextAction = "wait_for_legacy_work"
		applyMaintenanceGuidance(control)
		return commandResult{response: response, code: 1, err: publicSupervisorError("Supervisor maintenance is blocked.", err)}
	}
	control.State = "unavailable"
	control.MigrationReadiness = "unknown"
	control.FirstBlocker = &supervisorcore.MaintenanceBlocker{Kind: "evidence", ReasonCode: "maintenance_evidence_fault", NextAction: "stop_and_report"}
	control.NextAction = "stop_and_report"
	return commandResult{response: response, code: 1, err: publicSupervisorError("Delegate could not safely migrate the legacy supervisor.", err)}
}

func applyMaintenanceAssessment(control *SupervisorControlResponse, assessment supervisorcore.MaintenanceAssessment) {
	control.ActiveWork = assessment.ActiveWork
	control.MigrationReadiness = assessment.Readiness
	control.FirstBlocker = assessment.FirstBlocker
}

func applyMaintenanceGuidance(control *SupervisorControlResponse) {
	if control != nil && control.FirstBlocker != nil && control.MigrationReadiness != "clear" {
		control.NextAction = control.FirstBlocker.NextAction
	}
}

func observeSupervisorMaintenance(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job, control *SupervisorControlResponse) error {
	assessment, err := privateCleanupOwner().InspectMigrationQueue(ctx, store, supervisorcore.InspectionGroup(store.RootID, binding), jobs)
	applyMaintenanceAssessment(control, assessment)
	control.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if supervisorcore.MaintenanceBusy(err) {
		return nil
	}
	return err
}

func observeSupervisorMaintenanceAtRoot(ctx context.Context, root string, binding task.SupervisorRef, jobs []pueue.Job, control *SupervisorControlResponse) (resultErr error) {
	var store *taskdir.Store
	if err := openPrivateMigrationStore(ctx, root, &store); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	return observeSupervisorMaintenance(ctx, store, binding, jobs, control)
}

// A diagnostic after an RPC failure is a later observation, not a reconstruction
// of the resident's original cause. Preserve that cause and its result code.
func attachSupervisorDiagnostic(control, diagnostic *SupervisorControlResponse) {
	if diagnostic == nil || diagnostic.ObservedAt == "" {
		return
	}
	control.ActiveWork = diagnostic.ActiveWork
	control.MigrationReadiness = diagnostic.MigrationReadiness
	control.FirstBlocker = diagnostic.FirstBlocker
	control.DiagnosticSnapshotAt = diagnostic.ObservedAt
}

func supervisorMigrateWithoutInspectionGroupLocked(ctx context.Context, root string, lookup installation.ExecutableLookup, options pueue.Options) (*pueue.Client, error) {
	current, err := prepareCurrentPrivateSupervisor(ctx, root, lookup, options)
	if err != nil {
		return nil, err
	}
	pending, err := pueue.PrivateSupervisorMigrationPending(root)
	if err != nil {
		return current, err
	}
	if !pending {
		if err = current.VerifyPrivateMigrationEndpoint(ctx, root); err != nil {
			return current, err
		}
	}
	if err = current.MigratePrivateSupervisorWithLock(ctx, root); err != nil {
		return current, err
	}
	if err = current.StartPrivate(ctx, root); err != nil {
		return current, err
	}
	return current, nil
}

func failedSupervisor(response Response, message string, code int) commandResult {
	response.Error = message
	if response.SupervisorControl != nil && response.SupervisorControl.State == "unknown" {
		response.SupervisorControl.State = "unavailable"
	}
	return commandResult{response: response, code: code, err: errors.New(message)}
}

func failedSupervisorCause(response Response, message string, cause error, code int) commandResult {
	result := failedSupervisor(response, message, code)
	result.err = publicSupervisorError(message, cause)
	if result.response.SupervisorControl != nil {
		result.response.SupervisorControl.ReasonCode = supervisorcore.FailureReason(cause)
	}
	return result
}

func legacyActiveWork(snapshot pueue.QueueSnapshot) string {
	return supervisorcore.QueueActivity(snapshot.Jobs)
}

func supervisorEnsureOwner(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("supervisor")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	options := supervisorControlOptions(deps)
	ctx, cancel := supervisorcore.ControlContext(options)
	defer cancel()
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), true)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	control := supervisorcore.ControlRuntime{Options: options, Lookup: deps.supervisorLookup(), Routing: deps.infrastructureRouting()}
	client, err := control.PreparePrivateAdmission(ctx, store)
	if client != nil {
		err = errors.Join(err, client.Close())
	}
	response.RootID = store.RootID
	if err != nil {
		return failed(response, err, 1)
	}
	response.Status = "owner_ready"
	return commandResult{response: response}
}
