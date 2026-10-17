package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// recoverStagedAuxiliaryObservation owns the scheduling receipt/enqueue race
// for both inspections and workspace helpers. Each journal supplies exact
// receipt publication and verification; this function never grants a new add.
func RecoverStagedAuxiliaryObservation(ctx context.Context, supervisor *pueue.Client, root string, observation pueue.InspectionObservation, receiptCommit pueue.InspectionSubmissionCommit, receiptMatches func(int64) error, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	recovered, recoverErr := supervisor.RecoverInspectionStagedWithGate(ctx, observation.Identity, observation.Job.ID, SubmissionGate(root), receiptCommit)
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
