package inspection

import (
	"context"
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const workerObservationRecordName = "worker-observation.json"

type WorkerObservationRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	RequestSHA256    string `json:"request_sha256"`
	CompletionSHA256 string `json:"completion_sha256"`
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
	result, completion, err := o.ReadCompletedContext(ctx)
	if err != nil {
		return err
	}
	if result.Reason != ResultEligible {
		return task.ErrEvidenceFault
	}
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return err
	}
	if id != *receipt.NumericTaskID {
		return task.ErrEvidenceFault
	}
	data, err := task.MarshalCanonical(completion)
	if err != nil {
		return task.ErrEvidenceFault
	}
	_, err = o.control.PutContext(ctx, workerObservationRecordName, WorkerObservationRecord{
		SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, CompletionSHA256: task.ComputeSHA256(data), NumericTaskID: id, State: "succeeded",
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
	result, completion, err := o.ReadCompletedContext(ctx)
	if err != nil {
		return nil, err
	}
	if result.Reason != ResultEligible {
		return nil, task.ErrEvidenceFault
	}
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return nil, err
	}
	var observation WorkerObservationRecord
	err = withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(workerObservationRecordName, &observation)
	})
	if err != nil {
		return nil, err
	}
	data, err := task.MarshalCanonical(completion)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	if observation.SchemaVersion != task.SchemaVersion || observation.RequestSHA256 != o.digest || observation.CompletionSHA256 != task.ComputeSHA256(data) || observation.NumericTaskID != *receipt.NumericTaskID || observation.State != "succeeded" {
		return nil, task.ErrEvidenceFault
	}
	return canonicalResultFacts(result, o.factsLimit())
}

func canonicalResultFacts(result ResultRecord, limits ...int) (json.RawMessage, error) {
	data, err := task.MarshalCanonical(result.Facts)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	return canonicalFactsWithin(data, inspectionFactsLimit(limits))
}
