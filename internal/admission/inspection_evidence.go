package admission

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
	if task.RecordAbsent(err) {
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
	ready, err := SuccessfulInspectionLifetimeEvidence(ctx, operation)
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

func RecordedInspectionFacts(ctx context.Context, operation *inspection.Operation) (json.RawMessage, bool, bool, error) {
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
		return nil, true, true, ErrInspectionWorkerUnavailable
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
		return RecordedInspectionFacts(ctx, operation)
	}
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inspectionStatusWatchdog)
	defer cancel()
	facts, recorded, terminal, err := RecordedInspectionFacts(proofCtx, operation)
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
	if task.RecordAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var termination *task.LifetimeTerminationRecord
	if requireTermination {
		termination, err = operation.ReadLifetimeTerminationContext(ctx)
		if task.RecordAbsent(err) {
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
func SuccessfulInspectionLifetimeEvidence(ctx context.Context, operation *inspection.Operation) (bool, error) {
	historical, err := HistoricalInspectionLifetime(ctx, operation)
	if err != nil {
		return false, err
	}
	return completedInspectionEvidence(ctx, operation, !historical)
}

func HistoricalInspectionLifetime(ctx context.Context, operation *inspection.Operation) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if operation == nil {
		return false, task.ErrEvidenceFault
	}
	intent, err := guardian.OptionalIntent(operation.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return false, err
	}
	registration, err := guardian.OptionalRegistration(operation.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return false, err
	}
	return operation.Request().Binding.WorkerExitProof == "" && intent == nil && registration == nil, nil
}
