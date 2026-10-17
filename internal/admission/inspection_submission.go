package admission

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func SubmitInspectionOnce(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, runner, root string) error {
	_, err := submitInspectionOnceWithState(ctx, operation, supervisor, runner, root)
	return err
}

func submitInspectionOnceWithState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, runner, root string) (inspectionSubmissionState, error) {
	permit, err := operation.ClaimSubmissionContext(ctx, time.Now())
	if errors.Is(err, task.ErrAlreadySubmitted) {
		return inspectionSubmissionState{}, nil
	}
	if err != nil {
		return inspectionSubmissionState{}, err
	}
	record := operation.Request()
	// The worker executable is part of the immutable inspection binding. It was
	// fingerprinted before the durable claim, so revalidate the exact path and
	// bytes immediately before handing the launch to the supervisor. A changed
	// worker consumes the create-once claim but can never reach an external add.
	if err = supervisorcore.ValidateInspectionRunner(operation, runner); err != nil {
		return inspectionSubmissionState{}, err
	}
	launch := guardedInspectionLaunch(runner, root)
	identity := record.SupervisorIdentity()
	var observation pueue.InspectionObservation
	var submitErr error
	commit := func(observation pueue.InspectionObservation) error {
		return operation.RecordReceiptContext(ctx, observation.Job.ID)
	}
	if ConcurrentInspection(root, record.Binding) && record.Group == "default" {
		observation, submitErr = supervisor.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, supervisorcore.SubmissionGate(root), commit)
	} else {
		observation, submitErr = supervisor.SubmitInspectionStagedWithGate(ctx, permit, identity, launch, supervisorcore.SubmissionGate(root), commit)
	}
	if pending := supervisorcore.PendingInspectionCommand(submitErr); pending != nil {
		// SubmitInspection may have timed out in its verification, add, or
		// reconciliation command. Returning its pending owner keeps the
		// supervisor lease until completion without extending admission.
		return inspectionSubmissionState{}, submitErr
	}
	submission := inspectionSubmissionState{
		terminalEventRegistrationKnown: observation.TerminalEventRegistrationKnown,
		terminalEventRegistered:        observation.TerminalEventRegistered,
	}
	if observation.Pending != nil {
		if supervisorcore.PendingInspectionCommand(submitErr) == nil {
			submitErr = errors.Join(submitErr, &pueue.InFlightError{Pending: observation.Pending})
		}
		return submission, submitErr
	}
	if observation.Matched {
		return submission, nil
	}
	if ctx.Err() != nil {
		return submission, errors.Join(submitErr, ctx.Err())
	}
	// The durable guard is consumed even if add completion is unknown. Only
	// observational reconciliation follows; no failure path resubmits.
	return submission, nil
}
