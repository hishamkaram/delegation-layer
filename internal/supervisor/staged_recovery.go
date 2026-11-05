package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// recoverStagedAuxiliaryObservation owns the scheduling receipt/enqueue race
// for both inspections and workspace helpers. Each journal supplies exact
// receipt publication and verification; this function never grants a new add.
func RecoverStagedAuxiliaryObservation(ctx context.Context, supervisor *pueue.Client, root string, observation pueue.InspectionObservation, receiptCommit pueue.InspectionSubmissionCommit, receiptMatches func(int64) error, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	if supervisor != nil && supervisor.UsesControlOwner() {
		return observation, fmt.Errorf("%w: managed auxiliary recovery requires current launch permission", task.ErrInvalidPermit)
	}
	return recoverStagedAuxiliaryObservation(ctx, supervisor, root, observation, receiptCommit, receiptMatches, commit)
}

// Receipt replay remains historical evidence. Check current execution
// permission separately, after its transaction returns and directly before
// the canonical queue callback enqueues. The caller already owns outer RootSH.
func RecoverStagedAuxiliaryObservationChecked(ctx context.Context, supervisor *pueue.Client, root string, observation pueue.InspectionObservation, receiptCommit pueue.InspectionSubmissionCommit, receiptMatches func(int64) error, commit pueue.InspectionObservationCommit, check func(context.Context) error) (pueue.InspectionObservation, error) {
	if ctx == nil {
		return observation, context.Canceled
	}
	if check == nil || receiptCommit == nil {
		return observation, fmt.Errorf("%w: auxiliary recovery requires current launch permission", task.ErrInvalidPermit)
	}
	if err := check(ctx); err != nil {
		return observation, err
	}
	checkedCommit := func(current pueue.InspectionObservation) error {
		if err := receiptCommit(current); err != nil {
			return err
		}
		if err := check(ctx); err != nil {
			return auxiliaryLaunchPermissionError{cause: err}
		}
		return nil
	}
	return recoverStagedAuxiliaryObservation(ctx, supervisor, root, observation, checkedCommit, receiptMatches, commit)
}

type auxiliaryLaunchPermissionError struct{ cause error }

func (auxiliaryLaunchPermissionError) Error() string   { return "auxiliary launch permission refused" }
func (e auxiliaryLaunchPermissionError) Unwrap() error { return e.cause }

func recoverStagedAuxiliaryObservation(ctx context.Context, supervisor *pueue.Client, root string, observation pueue.InspectionObservation, receiptCommit pueue.InspectionSubmissionCommit, receiptMatches func(int64) error, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	recovered, recoverErr := supervisor.RecoverInspectionStagedWithGate(ctx, observation.Identity, observation.Job.ID, SubmissionGate(root), receiptCommit)
	var permissionErr auxiliaryLaunchPermissionError
	if errors.As(recoverErr, &permissionErr) {
		return recovered, recoverErr
	}
	if recoverErr == nil || PendingInspectionCommand(recoverErr) != nil || !errors.Is(recoverErr, pueue.ErrUnknown) {
		return recovered, recoverErr
	}
	if receiptErr := receiptMatches(observation.Job.ID); receiptErr != nil {
		return recovered, errors.Join(recoverErr, receiptErr)
	}
	// Another observer may have enqueued the exact receipted row between
	// the first Stashed observation and recovery. Reobserve under the
	// supervisor lease and run the original acceptance callback there so a
	// Running or Ended race cannot lose its application-side evidence.
	current, currentErr := supervisor.ReconcileInspectionWithLease(ctx, observation.Identity, commit)
	if currentErr != nil || !current.Matched || current.Job.ID != observation.Job.ID {
		return current, errors.Join(recoverErr, currentErr)
	}
	switch current.Job.State {
	case pueue.StateQueued, pueue.StateRunning, pueue.StateEnded:
		return current, nil
	case pueue.StateUnknown, pueue.StateStashed:
		return current, recoverErr
	}
	return current, recoverErr
}

func PendingInspectionCommand(err error) *pueue.Pending {
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil {
		return nil
	}
	return inFlight.Pending
}
