package taskdir

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// A session handoff witness is an immutable record proving that a claimed
// provider turn ended without an authoritative outcome. Two witness families
// exist: terminated budget-stop observations and the canonical closed native
// lifetime. A closure proves only that the old native scope ended; it is not
// provider success or a published outcome, and outcome.json remains the
// task-completion authority. Release writers prefer budget, then termination,
// then worker completion. A stored release stays valid while its digest remains
// a member of either family, so a later compatible record cannot revoke it.

// matchesHandoffProof reports whether digest is a current handoff witness.
// Each family is optional here: a malformed family must not invalidate a
// release proven by the other family or by the outcome. Explicit release
// writers read their family strictly instead.
func (td *TaskDir) matchesHandoffProofContext(ctx context.Context, digest string) (bool, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, err
	}
	budget, err := td.budgetHandoffDigests()
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err == nil && slices.Contains(budget, digest) {
		return true, nil
	}
	closure, err := td.lifetimeClosureHandoffDigestsContext(ctx)
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	return err == nil && slices.Contains(closure, digest), nil
}

// budgetHandoffDigests returns every terminated budget-stop observation digest
// in stop-record order.
func (td *TaskDir) budgetHandoffDigests() ([]string, error) {
	records, err := td.ReadStopRecords()
	if err != nil {
		return nil, err
	}
	var digests []string
	for _, record := range records {
		if record.Request == nil || !task.IsDeadlineStopCause(record.Request.Cause) || record.Observation == nil || !record.Observation.Terminated {
			continue
		}
		digest, err := canonicalRecordDigest(record.Observation)
		if err != nil {
			return nil, err
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

// lifetimeClosureHandoffDigests returns the canonical closure witnesses in
// release preference order: empty-scope termination, then worker completion.
// An open lifetime has no witness. task.ResolveLifetimeClosure owns closure.
func (td *TaskDir) lifetimeClosureHandoffDigestsContext(ctx context.Context) ([]string, error) {
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return nil, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || !closure.Closed() {
		return nil, err
	}
	var digests []string
	if evidence.Termination != nil {
		digest, err := canonicalRecordDigest(evidence.Termination)
		if err != nil {
			return nil, err
		}
		digests = append(digests, digest)
	}
	if evidence.WorkerWait != nil {
		digest, err := canonicalRecordDigest(evidence.WorkerWait)
		if err != nil {
			return nil, err
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

func (td *TaskDir) timeoutEvidenceDigest() (string, error) {
	digests, err := td.budgetHandoffDigests()
	if err != nil {
		return "", err
	}
	if len(digests) == 0 {
		return "", os.ErrNotExist
	}
	return digests[0], nil
}

// ReleaseSessionAfterLifetimeClosure transfers a continuation claim after the
// canonical native lifetime closed before the provider published an outcome,
// for example when a machine reboot ended the provider turn. The preferred
// closure witness is the handoff proof; the run lease must be released. This
// writer proves closure only: publication eligibility is the caller's admission
// policy, and a stored release must stay valid after a late outcome.
func (td *TaskDir) ReleaseSessionAfterLifetimeClosure(provider, conversation string) error {
	return td.ReleaseSessionAfterLifetimeClosureContext(context.Background(), provider, conversation)
}

func (td *TaskDir) ReleaseSessionAfterLifetimeClosureContext(ctx context.Context, provider, conversation string) error {
	digests, err := td.lifetimeClosureHandoffDigestsContext(ctx)
	if err != nil {
		return err
	}
	if len(digests) == 0 {
		return fmt.Errorf("%w: native lifetime is not closed", task.ErrSessionBusy)
	}
	return td.ReleaseSessionContext(ctx, provider, conversation, digests[0])
}

// handoffWitnessRecord is every record family that can prove a session handoff.
type handoffWitnessRecord interface {
	*task.StopObservedRecord | *task.LifetimeTerminationRecord | *task.LifetimeWorkerWaitRecord
}

func canonicalRecordDigest[T handoffWitnessRecord](record T) (string, error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}
