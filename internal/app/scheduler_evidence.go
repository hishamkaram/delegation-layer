package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func (tasks schedulerTasks) ReadResult(ctx context.Context, o contract.Occurrence) (result contract.ResultFact, resultErr error) {
	result = contract.ResultFact{TaskID: o.TaskID, ObservedAt: time.Now().UTC(), Verdict: "unknown"}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, tasks.root, tasks.dependencies.storeDependencies().registry())
	if err != nil {
		return result, err
	}
	defer func() { resultErr = schedulerTaskCleanupError(errors.Join(resultErr, reader.Close())) }()
	if reader.RootID() != tasks.rootID {
		return result, schedulerruntime.ErrRootIdentity
	}
	facts, observation, err := tasklifecycle.ReadPassiveTaskObservation(ctx, reader, o.TaskID)
	if err != nil {
		return result, err
	}
	return schedulerResultFact(o, facts, observation), nil
}

// schedulerResultFact selects public evidence without changing task state.
func schedulerResultFact(o contract.Occurrence, facts taskdir.TaskEvidence, observation tasklifecycle.PassiveTaskObservation) contract.ResultFact {
	result := contract.ResultFact{TaskID: o.TaskID, ObservedAt: time.Now().UTC(), Verdict: "unknown"}
	if !facts.Exists {
		if o.Release != nil && o.Release.Kind == contract.ReleaseNotCreated {
			result.Verdict = "not_created"
			result.Code = "admission_failed"
			if o.Attempt != nil {
				result.Code = o.Attempt.Code
			}
		}
		return result
	}
	if facts.Inspection != nil && facts.Inspection.Outcome != nil {
		outcome := facts.Inspection.Outcome
		result.OutcomeSHA256 = facts.OutcomeSHA256
		if observation.Publication == task.PublicationCommitted {
			result.Verdict = "committed"
			return result
		}
		result.Verdict = "rejected"
		result.Code = schedulerRejectionCode(outcome, o.Attempt)
		if result.Code == "provider_quota_exhausted" && outcome.Quota != nil {
			result.QuotaNotBefore = schedulerQuotaReset(outcome.Quota.NotBefore)
		}

		return result
	}
	switch observation.Readiness {
	case tasklifecycle.PassiveNeverStarted:
		result.Verdict = "not_created"
		result.Code = "task_not_started"
	case tasklifecycle.PassiveClosedWithoutOutcome:
		result.Verdict = "closed_without_outcome"
		result.Code = observation.FailureCode
		if result.Code == "" {
			result.Code = "closed_without_outcome"
		}
	case tasklifecycle.PassivePending, tasklifecycle.PassiveCollectRequired, tasklifecycle.PassivePublished:
	}
	return result
}

// Provider timestamps may be valid RFC3339 but outside the scheduler's signed
// nanosecond domain. Advice is optional: retain the quota diagnostic and omit
// the hold instead of faulting the scheduler for an absurd reset.
func schedulerQuotaReset(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || contract.ValidateInstant("quota reset", parsed) != nil {
		return time.Time{}
	}
	return parsed
}

func schedulerRejectionCode(outcome *task.OutcomeRecord, attempt *contract.AttemptFailure) string {
	var codes []string
	for _, diagnostic := range outcome.Diagnostics {
		if diagnostic.Code == "provider_quota_exhausted" {
			return contract.EffectiveFailureCode(diagnostic.Code, attempt)
		}
		codes = append(codes, diagnostic.Code)
	}
	canonicalCode := "provider_output_rejected"
	if len(codes) != 0 {
		sort.Strings(codes)
		canonicalCode = codes[0]
	}
	return contract.EffectiveFailureCode(canonicalCode, attempt)
}
