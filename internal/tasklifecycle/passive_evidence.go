package tasklifecycle

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PassiveTaskReader grants existing-evidence observation only. Implementations
// do not initialize storage, fence admission, publish, or consult a supervisor.
type PassiveTaskReader interface {
	ReadTaskEvidence(context.Context, string) (taskdir.TaskEvidence, error)
	ReadNeverStartedTaskContext(context.Context, string) (bool, string, error)
}

// PassiveReadiness describes why existing facts are sufficient to stop waiting.
// ClosedWithoutOutcome does not assert that a runner can never seal later.
type PassiveReadiness string

const (
	PassivePending              PassiveReadiness = "pending"
	PassivePublished            PassiveReadiness = "published"
	PassiveCollectRequired      PassiveReadiness = "collect_required"
	PassiveClosedWithoutOutcome PassiveReadiness = "closed_without_outcome"
	PassiveNeverStarted         PassiveReadiness = "never_started"
)

// PassiveTaskObservation is a derived view, not another outcome or lifetime
// authority. Liveness stays undetermined unless canonical closure is proven.
type PassiveTaskObservation struct {
	Admission   task.Admission
	Liveness    task.Liveness
	Publication task.Publication
	Readiness   PassiveReadiness
	Ready       bool
	Status      string
	FailureCode string
}

// ReadPassiveTaskObservation reuses validators and canonical noncreating proof.
// An absent task is reported separately by the caller, never as ready.
func ReadPassiveTaskObservation(ctx context.Context, reader PassiveTaskReader, id string) (taskdir.TaskEvidence, PassiveTaskObservation, error) {
	facts, err := reader.ReadTaskEvidence(ctx, id)
	if err != nil {
		return facts, PassiveTaskObservation{}, err
	}
	observation, err := ClassifyPassiveTaskEvidence(facts, "")
	if err != nil || observation.Ready || !facts.Exists || facts.Started != nil || facts.Lifetime.Intent != nil || facts.Lifetime.Registration != nil {
		return facts, observation, err
	}
	proven, reference, err := reader.ReadNeverStartedTaskContext(ctx, id)
	if err != nil {
		return facts, observation, err
	}
	if proven {
		observation, err = ClassifyPassiveTaskEvidence(facts, reference)
	}
	return facts, observation, err
}

// ClassifyPassiveTaskEvidence consumes validated facts and, optionally, the
// canonical complete never-started proof reference. Bare stop intent is not
// sufficient. The store owns validation of the reference and missing records.
func ClassifyPassiveTaskEvidence(facts taskdir.TaskEvidence, neverStartedReference string) (PassiveTaskObservation, error) {
	result := PassiveTaskObservation{Readiness: PassivePending}
	if !facts.Exists {
		return result, nil
	}
	result.Admission = task.AdmissionNotAdmitted
	if facts.Submission != nil {
		result.Admission = task.AdmissionUnknown
	}
	if facts.Receipt != nil {
		result.Admission = task.AdmissionAdmitted
	}
	if facts.Inspection != nil {
		result.Publication = facts.Inspection.Publication
		if result.Publication.Terminal() {
			return passivePublishedEvidence(facts, result)
		}
	}
	closure, err := task.ResolveLifetimeClosure(facts.Lifetime)
	if err != nil {
		return result, err
	}
	if closure.Closed() {
		return passiveClosedEvidence(facts, result), nil
	}
	if neverStartedReference != "" {
		return passiveNeverStartedEvidence(facts, neverStartedReference, result)
	}
	return result, nil
}

func passivePublishedEvidence(facts taskdir.TaskEvidence, result PassiveTaskObservation) (PassiveTaskObservation, error) {
	if facts.Inspection.Outcome == nil {
		return result, fmt.Errorf("%w: terminal publication without outcome", task.ErrEvidenceFault)
	}
	result.Ready, result.Readiness = true, PassivePublished
	if result.Publication == task.PublicationRejected {
		result.FailureCode = "provider_output_rejected"
	}
	return result, nil
}

// The caller has independently validated canonical lifetime closure. A seal
// changes the next read operation, without asserting accepted publication.
func passiveClosedEvidence(facts taskdir.TaskEvidence, result PassiveTaskObservation) PassiveTaskObservation {
	result.Ready, result.Liveness, result.Readiness = true, task.LivenessEnded, PassiveClosedWithoutOutcome
	if facts.Seal != nil {
		result.Readiness = PassiveCollectRequired
	}
	if termination := facts.Lifetime.Termination; termination != nil {
		switch termination.Cause {
		case task.LifetimeTerminationBudget:
			result.Status, result.FailureCode = "timed_out", "task_timed_out"
		case task.StopCauseOutputSilence:
			result.Status, result.FailureCode = "idle_timed_out", "task_idle_timed_out"
		}
	}
	return result
}

func passiveNeverStartedEvidence(facts taskdir.TaskEvidence, reference string, result PassiveTaskObservation) (PassiveTaskObservation, error) {
	if err := task.ValidateSHA256(reference); err != nil {
		return result, err
	}
	if facts.Started != nil || facts.Seal != nil || facts.Lifetime.Intent != nil || facts.Lifetime.Registration != nil || facts.Lifetime.Worker != nil || len(facts.Lifetime.Launches) != 0 {
		return result, fmt.Errorf("%w: never-started proof conflicts with execution", task.ErrEvidenceFault)
	}
	result.Ready, result.Readiness, result.Liveness = true, PassiveNeverStarted, task.LivenessEnded
	return result, nil
}
