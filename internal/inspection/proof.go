package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const workerObservationRecordName = "worker-observation.json"

type WorkerObservationRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	RequestSHA256    string `json:"request_sha256"`
	CompletionSHA256 string `json:"completion_sha256,omitempty"`
	NumericTaskID    int64  `json:"numeric_task_id"`
	State            string `json:"state"`
}

// RecordWorkerSuccess is called by the admission observer only after the bound
// supervisor reports the exact worker ended successfully. A worker cannot
// prove its own exit; result/completion records alone are insufficient.
func (o *Operation) RecordWorkerSuccess(id int64) error {
	return o.RecordWorkerSuccessContext(context.Background(), id)
}

// RecordWorkerSuccessContext records the independent supervisor success while
// honoring the caller's lifetime for every journal read and write.
func (o *Operation) RecordWorkerSuccessContext(ctx context.Context, id int64) error {
	return o.RecordWorkerExitContext(ctx, id, true)
}

// RecordWorkerExitContext preserves an independently observed terminal worker
// before its supervisor row can be removed. Completion alone is not exit proof.
func (o *Operation) RecordWorkerExitContext(ctx context.Context, id int64, succeeded bool) error {
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return err
	}
	if receipt.NumericTaskID == nil || id != *receipt.NumericTaskID {
		return task.ErrEvidenceFault
	}

	var completionSHA256 string
	result, completion, completionErr := o.ReadCompletedContext(ctx)
	if completionErr == nil {
		if succeeded && result.Reason != ResultEligible {
			return task.ErrEvidenceFault
		}
		data, marshalErr := task.MarshalCanonical(completion)
		if marshalErr != nil {
			return task.ErrEvidenceFault
		}
		completionSHA256 = task.ComputeSHA256(data)
	} else if succeeded || !errors.Is(completionErr, os.ErrNotExist) {
		return completionErr
	}
	state := "failed"
	if succeeded {
		state = "succeeded"
	}
	_, err = o.control.PutContext(ctx, workerObservationRecordName, WorkerObservationRecord{
		SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, CompletionSHA256: completionSHA256, NumericTaskID: id, State: state,
	})
	return err
}

// ReadProof is observational: it requires local completion and a separately
// recorded successful supervisor observation, all bound to the same target.
// Callers still revalidate profile sources, auth expiry and runtime identities.
func (o *Operation) ReadProof() (json.RawMessage, error) {
	return o.ReadProofContext(context.Background())
}

// ReadProofContext reads the complete admission proof while honoring the
// caller's lifetime for every journal transaction.
func (o *Operation) ReadProofContext(ctx context.Context) (json.RawMessage, error) {
	result, succeeded, err := o.ReadWorkerResultContext(ctx)
	if err != nil {
		return nil, err
	}
	if !succeeded || result == nil || result.Reason != ResultEligible {
		return nil, task.ErrEvidenceFault
	}
	return canonicalResultFacts(*result, o.factsLimit())
}

// ReadWorkerResultContext reads terminal local evidence and its independent
// supervisor observation. It remains usable after the supervisor row is reaped.
func (o *Operation) ReadWorkerResultContext(ctx context.Context) (*ResultRecord, bool, error) {
	if o == nil {
		return nil, false, task.ErrEvidenceFault
	}
	if o.request.Binding.WorkerExitProof == WorkerExitProofGuardianWaitV1 {
		return o.readManagedWorkerResultContext(ctx)
	}
	observation, err := o.readWorkerObservationContext(ctx)
	if err != nil {
		return nil, false, err
	}
	if observation.State == "failed" && observation.CompletionSHA256 == "" {
		return nil, false, nil
	}
	result, err := o.readObservedWorkerCompletion(ctx, observation)
	if err != nil {
		return nil, false, err
	}
	succeeded := observation.State == "succeeded"
	if succeeded && result.Reason != ResultEligible {
		return nil, false, task.ErrEvidenceFault
	}
	return &result, succeeded, nil
}

// readManagedWorkerResultContext derives managed exit authority from the
// guardian-owned worker completion disposition and independently validated
// empty-scope termination. Queue state is absent from this proof path.
func (o *Operation) readManagedWorkerResultContext(ctx context.Context) (*ResultRecord, bool, error) {
	if ctx == nil {
		return nil, false, context.Canceled
	}
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return nil, false, err
	}
	if receipt.NumericTaskID == nil {
		return nil, false, task.ErrEvidenceFault
	}
	waitRecord, err := o.ReadLifetimeWorkerWaitContext(ctx)
	if err != nil {
		return nil, false, err
	}
	termination, err := o.ReadLifetimeTerminationContext(ctx)
	if err != nil {
		return nil, false, lifetimeDependencyError(err)
	}
	if task.LifetimeWorkerWaitProvesNotStarted(waitRecord) {
		return managedWorkerFailureResult(termination.Cause)
	}
	if !task.LifetimeWorkerWaitIsQualified(waitRecord) {
		if waitRecord.Result == task.LifetimeWorkerWaitSuccess {
			return nil, false, task.ErrEvidenceFault
		}
		return managedWorkerFailureResult(termination.Cause)
	}
	return o.readStartedManagedWorkerResultContext(ctx, waitRecord, termination.Cause)
}

func (o *Operation) readStartedManagedWorkerResultContext(ctx context.Context, waitRecord *task.LifetimeWorkerWaitRecord, terminationCause string) (*ResultRecord, bool, error) {
	worker, err := o.ReadLifetimeWorkerContext(ctx)
	if err != nil {
		return nil, false, lifetimeDependencyError(err)
	}
	workerDigest, err := lifetimeRecordDigest(worker)
	if err != nil || waitRecord.WorkerSHA256 == "" || waitRecord.WorkerSHA256 != workerDigest || waitRecord.ChildPID != worker.PID {
		return nil, false, errors.Join(task.ErrEvidenceFault, err)
	}
	if waitRecord.Result != task.LifetimeWorkerWaitSuccess {
		return managedWorkerFailureResult(terminationCause)
	}
	result, _, err := o.ReadCompletedContext(ctx)
	if err != nil {
		return nil, false, lifetimeDependencyError(err)
	}
	if result.Reason != ResultEligible {
		return nil, false, task.ErrEvidenceFault
	}
	return &result, true, nil
}

func managedWorkerFailureResult(terminationCause string) (*ResultRecord, bool, error) {
	if terminationCause == task.LifetimeTerminationBudget {
		return nil, false, ErrAdmissionExpired
	}
	return nil, false, nil
}

func (o *Operation) readWorkerObservationContext(ctx context.Context) (WorkerObservationRecord, error) {
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return WorkerObservationRecord{}, err
	}
	var observation WorkerObservationRecord
	err = withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(workerObservationRecordName, &observation)
	})
	if err != nil {
		return WorkerObservationRecord{}, err
	}
	if receipt.NumericTaskID == nil || observation.SchemaVersion != task.SchemaVersion || observation.RequestSHA256 != o.digest || observation.NumericTaskID != *receipt.NumericTaskID || (observation.State != "succeeded" && observation.State != "failed") {
		return WorkerObservationRecord{}, task.ErrEvidenceFault
	}
	return observation, nil
}

func (o *Operation) readObservedWorkerCompletion(ctx context.Context, observation WorkerObservationRecord) (ResultRecord, error) {
	if task.ValidateSHA256(observation.CompletionSHA256) != nil {
		return ResultRecord{}, task.ErrEvidenceFault
	}
	result, completion, err := o.ReadCompletedContext(ctx)
	if err != nil {
		return ResultRecord{}, lifetimeDependencyError(err)
	}
	data, err := task.MarshalCanonical(completion)
	if err != nil {
		return ResultRecord{}, task.ErrEvidenceFault
	}
	if observation.CompletionSHA256 != task.ComputeSHA256(data) {
		return ResultRecord{}, task.ErrEvidenceFault
	}
	return result, nil
}

func canonicalResultFacts(result ResultRecord, limits ...int) (json.RawMessage, error) {
	data, err := task.MarshalCanonical(result.Facts)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	return canonicalFactsWithin(data, inspectionFactsLimit(limits))
}
