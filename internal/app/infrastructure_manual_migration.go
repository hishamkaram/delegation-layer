package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// The accepted operation owns executable selection. This fixed host runs the
// same canonical migration as the CLI with its resident guards, not a new
// migration protocol or an application Dependencies override.
func (infrastructureEntryHost) migrateOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, report infrastructure.ManualMigrationReport, resultErr error) {
	preparation, err := openInfrastructurePreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, report, err
	}
	state, migrationErr := inspectSupervisorMigration(ctx, preparation.store.Root, preparation.lookup, preparation.options, preparation.store)
	result := manualSupervisorMigrationResult(state, migrationErr)
	if result.err == nil && state.current != nil {
		preparation.observation.RecordReady()
	}
	report, reportErr := infrastructureManualMigrationReport(result)
	transitionErr := supervisor.ClosePrivateAdmissionTransition(state.current, result.err)
	return infrastructure.Disposition{}, report, errors.Join(transitionErr, reportErr)
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
