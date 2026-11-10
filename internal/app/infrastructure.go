package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/stop"
)

type nativeInfrastructureStores struct{}

func (nativeInfrastructureStores) OpenNative(ctx context.Context, root string) (*taskdir.Store, error) {
	return openNativeSupervisorStore(ctx, root)
}

func (nativeInfrastructureStores) RetainsStore(cause error, store *taskdir.Store) bool {
	return cleanupRetainsStore(cause, store)
}

func nativeInfrastructureCallbacks() infrastructurehost.Callbacks {
	return infrastructurehost.Callbacks{
		MaintenanceLaunch:      supervisor.LaunchQueueMaintenanceOwnerWithEnvironment,
		SuccessfulCleanup:      authorizePrivateSuccessfulCleanup,
		TerminalEvidence:       preparePrivateTerminalEvidence,
		TerminalCustody:        releasePrivateTerminalCustody,
		MigrationAuthorization: authorizePrivateMigrationQueue,
	}
}

func nativeInfrastructureCallbacksForReporter(report func(error)) infrastructurehost.Callbacks {
	callbacks := nativeInfrastructureCallbacks()
	if report != nil {
		cleanup := privateCleanupOwnerWithReporter(report)
		callbacks.SuccessfulCleanup = cleanup.AuthorizeSuccessfulCleanup
		callbacks.TerminalEvidence = cleanup.PrepareTerminalEvidence
		callbacks.TerminalCustody = func(ctx context.Context, root string, binding task.SupervisorRef, scope pueue.PrivateTerminalCustodyScope) error {
			return releasePrivateTerminalCustodyReported(ctx, root, binding, scope, report)
		}
		callbacks.MaintenanceLaunch = func(ctx context.Context, executable, root string, environment []string) error {
			_, err := supervisor.StartQueueMaintenanceOwnerWithReporter(ctx, executable, root, environment, report)
			return err
		}
	}
	return callbacks
}

func nativeInfrastructureHost() infrastructurehost.Host {
	return infrastructurehost.NewHost(nativeInfrastructureCallbacks(), nativeInfrastructureStores{}, nativeInfrastructureMigration{})
}

func (d Dependencies) infrastructureRouting() supervisor.OwnerRouting {
	if d.ownerRouting != nil {
		return d.ownerRouting
	}
	return infrastructurehost.NewRouting(d.invocationSource, d.RunnerExecutable)
}

func (d storeDependencies) infrastructureRouting() supervisor.OwnerRouting {
	if d.ownerRouting != nil {
		return d.ownerRouting
	}
	return infrastructurehost.NewRouting(d.invocationSource, d.runnerExecutable)
}

// Stop retains its existing exact external endpoint checks. Private recovery
// uses the committed stop locator and independent routing; an unknown contact
// cannot become a local daemon Start or a repeated queue mutation.
func (d Dependencies) supervisorControlClientOpener() stop.ControlClientOpener {
	routing := d.infrastructureRouting()
	return func(ctx context.Context, root string, saved task.SupervisorRef, source infrastructure.SavedBindingSource, options pueue.Options, lookup installation.ExecutableLookup) (*pueue.Client, error) {
		if !pueue.IsPrivateConfig(root, saved.ConfigPath) {
			return supervisor.NewControlClient(ctx, root, saved, options, lookup)
		}
		runtime := supervisor.ControlRuntime{Options: options, Lookup: lookup, Routing: routing, OpenStore: openNativeSupervisorStore}
		return runtime.OpenSaved(ctx, root, saved, source)
	}
}

type nativeInfrastructureMigration struct{}

func (nativeInfrastructureMigration) MigrateOwn(ctx context.Context, store *taskdir.Store, lookup installation.ExecutableLookup, options pueue.Options, observation *infrastructure.PreparationObservation) (infrastructure.ManualMigrationReport, error) {
	state, migrationErr := inspectSupervisorMigration(ctx, store.Root, lookup, options, store)
	result := manualSupervisorMigrationResult(state, migrationErr)
	report, reportErr := infrastructureManualMigrationReport(result)
	// The committed migration report survives an operational readiness fault.
	// Preparation owns its own transition after migration released bootstrap.
	var prepared *pueue.Client
	var preparationErr error
	if result.err == nil && state.current != nil {
		prepared, preparationErr = supervisor.PreparePrivateAdmissionSupervisorContext(ctx, lookup, store, options)
	}
	preparationErr = supervisor.ClosePrivateAdmissionTransition(prepared, preparationErr)
	if result.err == nil && reportErr == nil && preparationErr == nil && state.current != nil {
		observation.RecordReady()
	}
	transitionErr := supervisor.ClosePrivateAdmissionTransition(state.current, result.err)
	return report, errors.Join(transitionErr, reportErr, preparationErr)
}

func manualSupervisorMigrationResult(state supervisorMigrationState, err error) commandResult {
	response := newSupervisorCommandResponse("migrate")
	if err == nil && state.notInitialized {
		return notInitializedSupervisor(response)
	}
	return finishSupervisorMigration(response, state, err)
}

// Public projection owns migration report policy. Check the closed transport
// representation independently rather than deriving a second set of rules.
func infrastructureManualMigrationReport(result commandResult) (infrastructure.ManualMigrationReport, error) {
	control := result.response.SupervisorControl
	if control == nil {
		return infrastructure.ManualMigrationReport{}, infrastructure.ErrIdentity
	}
	report := infrastructure.ManualMigrationReport{
		State:  infrastructure.ManualMigrationState(control.State),
		Legacy: infrastructure.ManualMigrationLegacy(control.LegacyReachability),
		Active: infrastructure.ManualMigrationActive(control.ActiveWork),
	}
	if err := report.Validate(); err != nil {
		return infrastructure.ManualMigrationReport{}, err
	}
	return report, nil
}

// A completed report remains an observed fact even when later cleanup failed.
// Error policy still controls the exit code and whether another command is safe.
func finishRoutedSupervisorMigration(response Response, report infrastructure.ManualMigrationReport, cause error) commandResult {
	if report == (infrastructure.ManualMigrationReport{}) {
		return supervisorMigrationFailure(response, errors.Join(cause, infrastructure.ErrOwnerRequest))
	}
	if err := report.Validate(); err != nil {
		return supervisorMigrationFailure(response, errors.Join(cause, err))
	}
	if cause == nil {
		if err := report.ValidateCondition(infrastructure.PreparationOK); err != nil {
			return supervisorMigrationFailure(response, err)
		}
		if report.State == infrastructure.ManualMigrationNotInitialized {
			return notInitializedSupervisor(response)
		}
	}
	result := commandResult{response: response}
	if cause != nil {
		result = supervisorMigrationFailure(response, cause)
	} else {
		result.response.SupervisorControl.NextAction = "retry_dispatch"
	}
	control := result.response.SupervisorControl
	control.State = string(report.State)
	control.LegacyReachability = string(report.Legacy)
	control.ActiveWork = string(report.Active)
	if report.State == infrastructure.ManualMigrationMigrated {
		control.MigrationPhase = "complete"
	}
	return result
}

// SupervisorOwnerResponse describes only the selected infrastructure owner.
// Its serving release does not report Pueue or task termination.
type SupervisorOwnerResponse struct {
	Action string `json:"action"`
	infrastructure.OwnerStopResult
}

func supervisorOwnerStop(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("supervisor")
	response.SupervisorOwner = &SupervisorOwnerResponse{Action: "stop-owner", OwnerStopResult: infrastructure.OwnerStopResult{State: infrastructure.OwnerStopUnknown, ProcessClosure: infrastructure.OwnerProcessUnknown}}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner state root is unavailable", err), 1)
	}
	ctx, cancel := supervisor.ControlContext(supervisorControlOptions(deps))
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if onlyNotExist(err) {
		response.SupervisorOwner.State = infrastructure.OwnerNotInitialized
		return commandResult{response: response}
	}
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner state root is unavailable", err), 1)
	}
	defer func() { mergeCommandClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	observed, err := infrastructure.StopOwnerServing(ctx, reader)
	response.SupervisorOwner.OwnerStopResult = observed
	if err != nil {
		return failed(response, publicSupervisorError("infrastructure owner stop reported a control or observation fault", err), 1)
	}
	if observed.State == infrastructure.OwnerStopBusy {
		return commandResult{response: response, code: 3}
	}
	return commandResult{response: response}
}

func writeHumanSupervisorOwner(output *strings.Builder, owner *SupervisorOwnerResponse) {
	if owner == nil {
		return
	}
	fmt.Fprintf(output, "supervisor_owner action=%s state=%s process_closure=%s", owner.Action, owner.State, owner.ProcessClosure)
	if owner.Startup.Sequence != 0 {
		fmt.Fprintf(output, " startup_sequence=%d startup_epoch=%s", owner.Startup.Sequence, owner.Startup.Epoch)
	}
	output.WriteByte('\n')
}
