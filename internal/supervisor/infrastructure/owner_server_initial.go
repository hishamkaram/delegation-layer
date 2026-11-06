package infrastructure

import (
	"context"
	"errors"
	"time"
)

// The server mutex owns completion and delivery. Intent remains the sole owner
// of the immutable purpose, context and original deadline.
// The runtime prepares once; that call's defer publishes completion once.
func (s *ownerServer) completeInitial(outcome PreparationOutcome, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if outcome != (PreparationOutcome{}) {
		reference, referenceErr := ReferenceStartup(s.entry.operation.Intent())
		if referenceErr != nil {
			outcome, err = PreparationOutcome{}, errors.Join(err, referenceErr)
		} else {
			outcome.Startup = reference
		}
	}
	s.initialOutcome, s.initialError = outcome, err
	s.initialCompleted = true
	s.terminalHandoff = err != nil && !retainsResidentCleanup(err) && !s.closing && outcome != (PreparationOutcome{})
	close(s.initialDone)
}

func (s *ownerServer) matchesInitial(work acceptedOwnerWork) bool {
	return work.request.InitialResultFor == work.request.Owner && work.request.InitialResultFor != (StartupReference{}) &&
		work.request.preparationOperation().normalized() == work.operation.Intent().Inputs.Initial.normalized() &&
		work.selectedReference == work.operation.defaultContextReference()
}

func (s *ownerServer) checkInitialRequest(work acceptedOwnerWork) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if work.request.InitialResultFor != (StartupReference{}) && !s.matchesInitial(work) {
		return ErrIdentity
	}
	if s.terminalHandoff && !s.matchesInitial(work) {
		return errOwnerWorkBusy
	}
	return nil
}

func (s *ownerServer) preparationForRequest(ctx context.Context, work acceptedOwnerWork) (PreparationOutcome, bool, error) {
	if work.request.InitialResultFor == (StartupReference{}) {
		s.mu.Lock()
		terminal := s.terminalHandoff
		s.mu.Unlock()
		if terminal {
			return PreparationOutcome{Condition: PreparationBusy, Startup: work.request.Owner}, false, ErrOwnerBusy
		}
		outcome, err := s.executePreparation(ctx, work)
		return outcome, false, err
	}
	select {
	case <-s.initialDone:
	case <-ctx.Done():
		return s.initialFailure(ctx.Err()), false, ctx.Err()
	}
	s.mu.Lock()
	outcome, err := s.initialOutcome, s.initialError
	s.mu.Unlock()
	if outcome == (PreparationOutcome{}) {
		return outcome, false, err
	}
	if work.request.preparationOperation().normalized() == (InitialOperation{}) && err == nil {
		outcome, err = s.executePreparation(ctx, work)
		return outcome, false, err
	}
	return outcome, true, err
}

func (s *ownerServer) deliverInitialResult() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalHandoff && !s.deliveryCompleted {
		s.deliveryCompleted = true
		close(s.initialDelivered)
	}
}

// Failed or partial writes leave delivery pending. Existing admitted writers
// drain normally after the runtime stops new admission.
func (s *ownerServer) awaitInitialHandoff(ctx context.Context) bool {
	s.mu.Lock()
	terminal := s.terminalHandoff
	s.mu.Unlock()
	if !terminal {
		return false
	}
	deadline, err := canonicalStartupTime(s.entry.operation.Intent().Deadline)
	if err != nil {
		return false
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.initialDelivered:
	case <-s.admissionStopped:
	case <-timer.C:
	case <-ctx.Done():
	}
	return true
}
