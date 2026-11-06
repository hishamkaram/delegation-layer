package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// This stateless edge borrows the existing invocation's readers and producer.
// Canonical records own selection; no contact result becomes a mutable head.
type infrastructurePreparationContact struct {
	routing   infrastructureOwnerRouting
	resources *infrastructureRoutingResources
	selection infrastructureRoutingSelection
}

// The application owns the one-refresh decision. Effects retain their existing
// record, serving, producer, request and process-observation owners.
type infrastructurePreparationEffects interface {
	ReadArtifacts(context.Context, infrastructure.StartupReference) (infrastructure.Artifacts, error)
	ReleaseServing(context.Context, infrastructure.StartupReference) (infrastructure.OwnerStopResult, error)
	EstablishReplacement(context.Context) (infrastructure.EstablishedOwner, error)
	Request(context.Context, infrastructure.EstablishedOwner) (infrastructure.PreparationOutcome, error)
	WaitProcessClosed(context.Context, infrastructure.StartupReference) error
}

func requestInfrastructurePreparation(ctx context.Context, initial infrastructure.InitialOperation, established infrastructure.EstablishedOwner, effects infrastructurePreparationEffects) (infrastructure.PreparationOutcome, error) {
	var retired infrastructure.StartupReference
	if initial.Kind == infrastructure.InitialMigrateOwn && established.InitialResultFor == (infrastructure.StartupReference{}) {
		var err error
		established, retired, err = refreshInfrastructureMigrationOwner(ctx, established, effects)
		if err != nil {
			return infrastructure.PreparationOutcome{}, err
		}
	}
	outcome, err := effects.Request(ctx, established)
	// A refusal can leave D1 live, with A correctly retaining its actual Wait.
	// Only a migrated report requires this command to observe A's process closure.
	if retired != (infrastructure.StartupReference{}) && outcome.Report.State == infrastructure.ManualMigrationMigrated {
		err = errors.Join(err, effects.WaitProcessClosed(ctx, retired))
	}
	return outcome, errors.Join(infrastructurePreparationOutcomeError(outcome), err)
}

func refreshInfrastructureMigrationOwner(ctx context.Context, established infrastructure.EstablishedOwner, effects infrastructurePreparationEffects) (infrastructure.EstablishedOwner, infrastructure.StartupReference, error) {
	missing, helperErr := infrastructureOwnerHelperMissing(ctx, established.Owner, effects)
	if !missing {
		if helperErr != nil {
			return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, helperErr
		}
		return established, infrastructure.StartupReference{}, nil
	}
	released, err := effects.ReleaseServing(ctx, established.Owner)
	if err != nil {
		return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, errors.Join(helperErr, err)
	}
	if released.State == infrastructure.OwnerStopBusy {
		return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, infrastructure.ErrOwnerBusy
	}
	if released.Startup != established.Owner || released.State != infrastructure.OwnerServingReleased {
		return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, infrastructure.ErrIdentity
	}
	retired := established.Owner
	replacement, err := effects.EstablishReplacement(ctx)
	if err != nil {
		return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, err
	}
	if err = replacement.Validate(); err != nil {
		return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, err
	}
	if replacement.InitialResultFor == (infrastructure.StartupReference{}) {
		// A concurrent O2 may win the one election. Contact it only after its
		// helper is checked; another missing helper never permits a second release.
		if _, err = infrastructureOwnerHelperMissing(ctx, replacement.Owner, effects); err != nil {
			return infrastructure.EstablishedOwner{}, infrastructure.StartupReference{}, err
		}
	}
	return replacement, retired, nil
}

func infrastructureOwnerHelperMissing(ctx context.Context, owner infrastructure.StartupReference, effects infrastructurePreparationEffects) (bool, error) {
	artifacts, err := effects.ReadArtifacts(ctx, owner)
	if err != nil {
		// Missing canonical records are not proof of a missing installed helper.
		return false, err
	}
	_, err = supervisor.ResolveRunner(artifacts.Runner, "")
	return errors.Is(err, os.ErrNotExist), err
}

func (c infrastructurePreparationContact) ReadArtifacts(ctx context.Context, owner infrastructure.StartupReference) (infrastructure.Artifacts, error) {
	return infrastructure.ReadOwnerArtifacts(ctx, c.resources.reader, owner)
}

func (c infrastructurePreparationContact) ReleaseServing(ctx context.Context, owner infrastructure.StartupReference) (infrastructure.OwnerStopResult, error) {
	if err := c.preflightReplacement(ctx); err != nil {
		return infrastructure.OwnerStopResult{}, err
	}
	return infrastructure.StopOwnerServingAt(ctx, c.resources.reader, owner)
}

func (c infrastructurePreparationContact) preflightReplacement(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, nested := c.routing.invocation.source.Origin(); nested {
		return fmt.Errorf("%w: missing infrastructure helper replacement requires an outside invocation", infrastructure.ErrOwnerRequest)
	}
	host := newInfrastructureEstablishmentHost(c.routing.defaultRunner, c.selection.initial)
	if _, err := host.PrepareOutsideRunner(ctx, c.resources.target); err != nil {
		return err
	}
	return ctx.Err()
}

func (c infrastructurePreparationContact) EstablishReplacement(ctx context.Context) (infrastructure.EstablishedOwner, error) {
	pin, err := c.resources.target.PinRootContext(ctx)
	if err != nil {
		return infrastructure.EstablishedOwner{}, err
	}
	if err = requireInfrastructureRoutingPin(ctx, c.resources.reader, pin); err != nil {
		return infrastructure.EstablishedOwner{}, err
	}
	selection := c.selection
	selection.owner = infrastructure.StartupReference{}
	return c.routing.establish(ctx, c.resources, pin, selection, newInfrastructureEstablishmentHost(c.routing.defaultRunner, selection.initial))
}

func (c infrastructurePreparationContact) Request(ctx context.Context, established infrastructure.EstablishedOwner) (infrastructure.PreparationOutcome, error) {
	return infrastructure.RequestBoundPreparation(ctx, c.resources.reader, established.Owner, infrastructure.PreparationRequest{
		Operation: c.selection.initial, Context: c.selection.reference, InitialResultFor: established.InitialResultFor,
	})
}

func (c infrastructurePreparationContact) WaitProcessClosed(ctx context.Context, owner infrastructure.StartupReference) error {
	_, err := infrastructure.WaitOwnerProcessClosed(ctx, c.resources.reader, owner)
	return err
}

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
