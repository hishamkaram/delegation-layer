package infrastructure

import (
	"context"
	"errors"
)

// The fixed dispatcher is shared by automatic startup and explicit fresh RPCs.
// Host cleanup has returned before classification or outcome publication.
func (s *ownerServer) dispatchPreparation(ctx context.Context, work acceptedOwnerWork) (Disposition, ManualMigrationReport, RuntimeRestartReport, error) {
	if err := validateRuntimeRestartRequest(work.operation, work.request); err != nil {
		return Disposition{}, ManualMigrationReport{}, RuntimeRestartReport{}, err
	}
	selected := work.selected.Clone()
	switch work.request.Action {
	case ownerEnsureOwn:
		d, err := s.host.EnsureOwnRoot(ctx, work.operation, selected, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, RuntimeRestartReport{}, err
	case ownerPrepareContinuation:
		d, err := s.host.PrepareContinuation(ctx, work.operation, selected, work.request.Source, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, RuntimeRestartReport{}, err
	case ownerRecoverSaved:
		d, err := s.host.RecoverSaved(ctx, work.operation, selected, work.request.Source, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, RuntimeRestartReport{}, err
	case ownerMigrateOwn:
		d, r, err := s.host.MigrateOwnRoot(ctx, work.operation, selected, s.entry.grant, s.waits)
		return d, r, RuntimeRestartReport{}, err
	case ownerRuntimeRestart:
		d, r, err := s.host.PrepareRuntimeRestart(ctx, work.operation, selected, work.request.RestartFrom, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, r, err
	case ownerStatus, ownerEnsureOther, ownerRuntimeRestartPreflight:
		return Disposition{}, ManualMigrationReport{}, RuntimeRestartReport{}, ErrIdentity
	}
	return Disposition{}, ManualMigrationReport{}, RuntimeRestartReport{}, ErrIdentity
}

func (s *ownerServer) executePreparation(ctx context.Context, work acceptedOwnerWork) (result PreparationOutcome, resultErr error) {
	if work.request.Action == ownerRuntimeRestart {
		defer func() { s.observeRuntimePreparation(work.request.RestartFrom, result, resultErr) }()
	}
	d, r, restart, err := s.dispatchPreparation(ctx, work)
	outcome := PreparationOutcome{Condition: PreparationOK, Startup: work.request.Owner, Disposition: d, Report: r, RuntimeRestart: restart}
	if invalid := validatePreparedObservation(outcome, work); invalid != nil {
		s.connectionFault(errors.Join(err, invalid))
		return PreparationOutcome{}, errors.Join(err, invalid)
	}
	if err == nil && (d == (Disposition{}) && r == (ManualMigrationReport{}) && restart == (RuntimeRestartReport{}) || work.request.Action == ownerMigrateOwn && r == (ManualMigrationReport{}) || work.request.Action == ownerRuntimeRestart && restart == (RuntimeRestartReport{})) {
		err = ErrIdentity
	}
	if err != nil {
		reply := ownerReply{Action: work.request.Action, Startup: work.request.Owner, Report: r, RuntimeRestart: restart}
		s.failReply(&reply, err)
		outcome.Condition = PreparationCondition(reply.Code)
		outcome.Failure = s.host.ClassifyPreparation(err)
	}
	if invalid := outcome.Validate(); invalid != nil {
		s.connectionFault(errors.Join(err, invalid))
		return PreparationOutcome{}, errors.Join(err, invalid)
	}
	if retainsResidentCleanup(err) && !outcome.Failure.IndependentFault {
		return PreparationOutcome{}, errors.Join(err, ErrIdentity)
	}
	return outcome, err
}

func validatePreparedObservation(outcome PreparationOutcome, work acceptedOwnerWork) error {
	if work.request.Action == ownerRuntimeRestart || outcome.RuntimeRestart != (RuntimeRestartReport{}) {
		if err := outcome.RuntimeRestart.ValidateDisposition(outcome.Disposition); err != nil {
			return err
		}
	}
	if outcome.Disposition != (Disposition{}) {
		if err := validateOwnerServerDisposition(outcome.Disposition, work.selectedReference); err != nil {
			return err
		}
	}
	if outcome.Report != (ManualMigrationReport{}) {
		if work.request.Action != ownerMigrateOwn || outcome.RuntimeRestart != (RuntimeRestartReport{}) {
			return ErrIdentity
		}
		return outcome.Report.Validate()
	}
	if outcome.RuntimeRestart != (RuntimeRestartReport{}) {
		if work.request.Action != ownerRuntimeRestart || outcome.RuntimeRestart.From != work.request.RestartFrom {
			return ErrIdentity
		}
		return outcome.RuntimeRestart.Validate()
	}
	return nil
}

func (s *ownerServer) initialFailure(err error) PreparationOutcome {
	if err == nil {
		return PreparationOutcome{}
	}
	reply := ownerReply{}
	s.failReply(&reply, err)
	outcome := PreparationOutcome{Condition: PreparationCondition(reply.Code), Failure: s.host.ClassifyPreparation(err)}
	if outcome.Validate() != nil || retainsResidentCleanup(err) && !outcome.Failure.IndependentFault {
		return PreparationOutcome{}
	}
	return outcome
}
