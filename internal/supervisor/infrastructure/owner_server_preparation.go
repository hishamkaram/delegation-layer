package infrastructure

import (
	"context"
	"errors"
)

// The fixed dispatcher is shared by automatic startup and explicit fresh RPCs.
// Host cleanup has returned before classification or outcome publication.
func (s *ownerServer) dispatchPreparation(ctx context.Context, work acceptedOwnerWork) (Disposition, ManualMigrationReport, error) {
	selected := work.selected.Clone()
	switch work.request.Action {
	case ownerEnsureOwn:
		d, err := s.host.EnsureOwnRoot(ctx, work.operation, selected, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, err
	case ownerPrepareContinuation:
		d, err := s.host.PrepareContinuation(ctx, work.operation, selected, work.request.Source, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, err
	case ownerRecoverSaved:
		d, err := s.host.RecoverSaved(ctx, work.operation, selected, work.request.Source, s.entry.grant, s.waits)
		return d, ManualMigrationReport{}, err
	case ownerMigrateOwn:
		d, r, err := s.host.MigrateOwnRoot(ctx, work.operation, selected, s.entry.grant, s.waits)
		return d, r, err
	case ownerStatus, ownerEnsureOther:
		return Disposition{}, ManualMigrationReport{}, ErrIdentity
	}
	return Disposition{}, ManualMigrationReport{}, ErrIdentity
}

func (s *ownerServer) executePreparation(ctx context.Context, work acceptedOwnerWork) (PreparationOutcome, error) {
	d, r, err := s.dispatchPreparation(ctx, work)
	outcome := PreparationOutcome{Condition: PreparationOK, Startup: work.request.Owner, Disposition: d, Report: r}
	if invalid := validatePreparedObservation(outcome, work); invalid != nil {
		s.connectionFault(errors.Join(err, invalid))
		return PreparationOutcome{}, errors.Join(err, invalid)
	}
	if err == nil && (d == (Disposition{}) && r == (ManualMigrationReport{}) || work.request.Action == ownerMigrateOwn && r == (ManualMigrationReport{})) {
		err = ErrIdentity
	}
	if err != nil {
		reply := ownerReply{Report: r}
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
	if outcome.Disposition != (Disposition{}) {
		if err := validateOwnerServerDisposition(outcome.Disposition, work.selectedReference); err != nil {
			return err
		}
	}
	if outcome.Report != (ManualMigrationReport{}) {
		if work.request.Action != ownerMigrateOwn {
			return ErrIdentity
		}
		return outcome.Report.Validate()
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
