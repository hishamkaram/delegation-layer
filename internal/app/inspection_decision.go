package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type inspectionAdmissionDecision uint8

const (
	inspectionAdmissionPending inspectionAdmissionDecision = iota
	inspectionAdmissionEligible
	inspectionAdmissionUnavailable
)

type inspectionAdmissionEvaluation struct {
	decision inspectionAdmissionDecision
	facts    json.RawMessage
	recorded bool
}

// evaluateInspectionAdmission is the sole authority for turning immutable
// inspection records into an admission decision. It is observational: native
// lifetime recovery, supervisor cleanup, and queue maintenance are separate
// operations and cannot change a decision made from durable proof.
func evaluateInspectionAdmission(ctx context.Context, operation *inspection.Operation) (inspectionAdmissionEvaluation, error) {
	if ctx == nil {
		return inspectionAdmissionEvaluation{}, context.Canceled
	}
	if operation == nil {
		return inspectionAdmissionEvaluation{}, task.ErrEvidenceFault
	}
	result, succeeded, err := operation.ReadWorkerResultContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return inspectionAdmissionEvaluation{decision: inspectionAdmissionPending}, nil
	}
	if err != nil {
		return inspectionAdmissionEvaluation{}, err
	}
	if !succeeded {
		return inspectionAdmissionEvaluation{decision: inspectionAdmissionUnavailable, recorded: true}, nil
	}
	if result == nil || result.Reason != inspection.ResultEligible {
		return inspectionAdmissionEvaluation{}, task.ErrEvidenceFault
	}
	ready, err := successfulInspectionLifetimeEvidence(ctx, operation)
	if err != nil {
		return inspectionAdmissionEvaluation{}, err
	}
	if !ready {
		return inspectionAdmissionEvaluation{decision: inspectionAdmissionPending, recorded: true}, nil
	}
	facts, err := operation.ReadProofContext(ctx)
	if err != nil {
		return inspectionAdmissionEvaluation{}, err
	}
	return inspectionAdmissionEvaluation{
		decision: inspectionAdmissionEligible,
		facts:    facts,
		recorded: true,
	}, nil
}

func recordedInspectionFacts(ctx context.Context, operation *inspection.Operation) (json.RawMessage, bool, bool, error) {
	evaluation, err := evaluateInspectionAdmission(ctx, operation)
	if err != nil {
		return nil, evaluation.recorded, false, err
	}
	switch evaluation.decision {
	case inspectionAdmissionPending:
		return nil, evaluation.recorded, false, nil
	case inspectionAdmissionEligible:
		return evaluation.facts, true, true, nil
	case inspectionAdmissionUnavailable:
		return nil, true, true, errInspectionWorkerUnavailable
	default:
		return nil, evaluation.recorded, false, task.ErrEvidenceFault
	}
}

// recordedInspectionFactsAfterObservation gives durable local proof one final
// bounded read after an external status command returns. A canceled status
// caller cannot prevent already-written proof from winning, and an absent
// proof cannot replace the original observation error with context.Canceled.
func recordedInspectionFactsAfterObservation(ctx context.Context, operation *inspection.Operation) (json.RawMessage, bool, bool, error) {
	if ctx == nil || ctx.Err() == nil {
		return recordedInspectionFacts(ctx, operation)
	}
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inspectionStatusWatchdog)
	defer cancel()
	facts, recorded, terminal, err := recordedInspectionFacts(proofCtx, operation)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, recorded, false, nil
	}
	return facts, recorded, terminal, err
}

func timelyCompletedInspectionEvidence(ctx context.Context, operation *inspection.Operation) (bool, error) {
	return completedInspectionEvidence(ctx, operation, true)
}

func completedInspectionEvidence(ctx context.Context, operation *inspection.Operation, requireTermination bool) (bool, error) {
	result, completion, err := operation.ReadCompletedContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var termination *task.LifetimeTerminationRecord
	if requireTermination {
		termination, err = operation.ReadLifetimeTerminationContext(ctx)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	if err = validateCompletedInspectionEvidence(operation.Deadline(), result, completion, termination, requireTermination); err != nil {
		return false, err
	}
	return true, nil
}

// successfulInspectionLifetimeEvidence validates durable lifetime proof only.
// Guardian-Wait workers always require empty-scope termination. A request
// saved without that immutable contract and without a lifetime journal keeps
// its historical queue-observation path.
func successfulInspectionLifetimeEvidence(ctx context.Context, operation *inspection.Operation) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if operation == nil {
		return false, task.ErrEvidenceFault
	}
	intent, err := optionalLifetimeIntent(operation.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return false, err
	}
	registration, err := optionalLifetimeRegistration(operation.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return false, err
	}
	historical := operation.Request().Binding.WorkerExitProof == "" && intent == nil && registration == nil
	return completedInspectionEvidence(ctx, operation, !historical)
}

func validateTimelyCompletedInspectionEvidence(deadline time.Time, result inspection.ResultRecord, completion inspection.CompletionRecord, termination *task.LifetimeTerminationRecord) error {
	return validateCompletedInspectionEvidence(deadline, result, completion, termination, true)
}

func validateCompletedInspectionEvidence(deadline time.Time, result inspection.ResultRecord, completion inspection.CompletionRecord, termination *task.LifetimeTerminationRecord, requireTermination bool) error {
	if result.Reason != inspection.ResultEligible || (requireTermination && termination == nil) {
		return inspection.ErrAdmissionExpired
	}
	completedAt, err := time.Parse(time.RFC3339Nano, completion.CompletedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if !completedAt.Before(deadline) {
		return inspection.ErrAdmissionExpired
	}
	if !requireTermination {
		return nil
	}
	terminatedAt, err := time.Parse(time.RFC3339Nano, termination.ObservedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if termination.Cause != task.LifetimeTerminationCompleted || !termination.ScopeEmpty || !terminatedAt.Before(deadline) {
		return inspection.ErrAdmissionExpired
	}
	return nil
}
