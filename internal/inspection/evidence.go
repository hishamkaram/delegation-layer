package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	receiptRecordName    = "receipt.json"
	resultRecordName     = "result.json"
	completionRecordName = "completion.json"
)

// ResultReason is deliberately closed: native errors and arbitrary diagnostics
// cannot be substituted into persisted inspection evidence.
type ResultReason string

const (
	ResultEligible    ResultReason = "eligible"
	ResultUnavailable ResultReason = "unavailable"
	ResultExpired     ResultReason = "deadline-expired"
)

type ReceiptRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	NumericTaskID *int64 `json:"numeric_task_id"`
}

type ResultRecord struct {
	SchemaVersion int                        `json:"schema_version"`
	RequestSHA256 string                     `json:"request_sha256"`
	Reason        ResultReason               `json:"reason"`
	Facts         map[string]json.RawMessage `json:"facts"`
}

// CompletionRecord proves completed local inspection work, not supervisor
// termination. Eligibility also requires the exact worker's successful exit.
type CompletionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	ResultSHA256  string `json:"result_sha256"`
	CompletedAt   string `json:"completed_at"`
	NativeExit    string `json:"native_exit"`
}

// RecordReceipt records an exact numeric target only after the caller has
// reconciled this operation's immutable label, group and supervisor binding.
func (o *Operation) RecordReceipt(id int64) error {
	return o.RecordReceiptContext(context.Background(), id)
}

// RecordReceiptContext records an exact numeric target while honoring the
// caller's lifetime. A canceled caller cannot publish a receipt authority.
func (o *Operation) RecordReceiptContext(ctx context.Context, id int64) error {
	if o == nil || o.control == nil || id < 0 {
		return task.ErrEvidenceFault
	}
	record := ReceiptRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, NumericTaskID: &id}
	_, err := o.control.PutContext(ctx, receiptRecordName, record)
	return err
}

func (o *Operation) Receipt() (ReceiptRecord, error) {
	return o.ReceiptContext(context.Background())
}

// ReceiptContext reads the saved numeric target within the caller's lifetime.
func (o *Operation) ReceiptContext(ctx context.Context) (ReceiptRecord, error) {
	if o == nil || o.control == nil {
		return ReceiptRecord{}, os.ErrClosed
	}
	var record ReceiptRecord
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(receiptRecordName, &record)
	})
	if err != nil {
		return ReceiptRecord{}, classifyReceiptReadError(err)
	}
	if err := validateReceiptRecord(record, o.digest); err != nil {
		return ReceiptRecord{}, task.ErrEvidenceFault
	}
	return record, nil
}

func classifyReceiptReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return task.ErrEvidenceFault
}

func validateReceiptRecord(record ReceiptRecord, digest string) error {
	if record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != digest || record.NumericTaskID == nil || *record.NumericTaskID < 0 {
		return task.ErrEvidenceFault
	}
	return nil
}

// Complete persists result before completion using the shared create-once
// journal primitive. A failed completion write leaves an ineligible payload.
// The worker invokes this only after joining native and network work.
func (o *Operation) Complete(reason ResultReason, facts json.RawMessage, now time.Time) error {
	if o == nil || o.control == nil || now.IsZero() {
		return task.ErrEvidenceFault
	}
	result, err := newResultRecord(reason, facts, o.digest)
	if err != nil {
		return err
	}
	if reason == ResultEligible && o.Expired(now) {
		return ErrAdmissionExpired
	}
	return withControlTransaction(o.control, func(tx *taskdir.ControlTransaction) error {
		if stopErr := rejectStoppedEligibility(tx, reason); stopErr != nil {
			return stopErr
		}
		if startErr := o.validateStarted(tx, now); startErr != nil {
			return startErr
		}
		resultBytes, marshalErr := task.MarshalCanonical(result)
		if marshalErr != nil {
			return task.ErrEvidenceFault
		}
		completion := CompletionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, ResultSHA256: task.ComputeSHA256(resultBytes), CompletedAt: canonicalNow(now), NativeExit: nativeExitClassification(reason)}
		var existing CompletionRecord
		if readErr := tx.Read(completionRecordName, &existing); readErr == nil {
			if validationErr := o.validateCompletion(existing, result); validationErr != nil {
				return validationErr
			}
			completion = existing
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if _, putErr := tx.Put(resultRecordName, result); putErr != nil {
			return putErr
		}
		_, putErr := tx.Put(completionRecordName, completion)
		return putErr
	})
}

func rejectStoppedEligibility(tx *taskdir.ControlTransaction, reason ResultReason) error {
	if reason != ResultEligible {
		return nil
	}
	var stop StopRequestRecord
	err := tx.Read(stopRequestRecordName, &stop)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return task.ErrEvidenceFault
	}
	return ErrAdmissionExpired
}

func newResultRecord(reason ResultReason, facts json.RawMessage, digest string) (ResultRecord, error) {
	result := ResultRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: digest, Reason: reason, Facts: map[string]json.RawMessage{}}
	if reason == ResultEligible || len(facts) != 0 {
		if task.ValidateJSONStructure(facts) != nil || json.Unmarshal(facts, &result.Facts) != nil || result.Facts == nil {
			return ResultRecord{}, task.ErrEvidenceFault
		}
	}
	if _, err := validateInspectionResult(result, digest); err != nil {
		return ResultRecord{}, err
	}
	return result, nil
}

func (o *Operation) validateStarted(tx *taskdir.ControlTransaction, completed time.Time) error {
	var start StartRecord
	if err := tx.Read(startRecordName, &start); err != nil {
		return err
	}
	if err := validateClaim(start); err != nil || start.RequestSHA256 != o.digest {
		return task.ErrEvidenceFault
	}
	started, err := canonicalTimestamp("inspection start", start.CreatedAt)
	if err != nil || completed.Before(started) || !started.Before(o.deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateInspectionResult(record ResultRecord, digest string) (json.RawMessage, error) {
	if record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != digest {
		return nil, task.ErrEvidenceFault
	}
	switch record.Reason {
	case ResultEligible:
		data, err := task.MarshalCanonical(record.Facts)
		if err != nil {
			return nil, task.ErrEvidenceFault
		}
		facts, err := canonicalProjectionFacts(data)
		if err != nil {
			return nil, task.ErrEvidenceFault
		}
		return facts, nil
	case ResultUnavailable, ResultExpired:
		if len(record.Facts) != 0 {
			return nil, task.ErrEvidenceFault
		}
		return nil, nil
	default:
		return nil, task.ErrEvidenceFault
	}
}

func (o *Operation) validateCompletion(completion CompletionRecord, result ResultRecord) error {
	if completion.SchemaVersion != task.SchemaVersion || completion.RequestSHA256 != o.digest || completion.NativeExit != nativeExitClassification(result.Reason) {
		return task.ErrEvidenceFault
	}
	data, err := task.MarshalCanonical(result)
	if err != nil || completion.ResultSHA256 != task.ComputeSHA256(data) {
		return task.ErrEvidenceFault
	}
	completed, err := canonicalTimestamp("inspection completion", completion.CompletedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	created, err := canonicalTimestamp("inspection creation", o.request.CreatedAt)
	if err != nil || completed.Before(created) {
		return task.ErrEvidenceFault
	}
	if result.Reason == ResultEligible && !completed.Before(o.deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func nativeExitClassification(reason ResultReason) string {
	if reason == ResultEligible {
		return "successful-exit"
	}
	return "unavailable"
}

// ReadCompleted returns only internally consistent, completed local evidence.
// The caller must separately observe a matching successful supervisor job
// before treating eligible facts as admission proof. This method never launches.
func (o *Operation) ReadCompleted() (ResultRecord, CompletionRecord, error) {
	return o.ReadCompletedContext(context.Background())
}

// ReadCompletedContext returns internally consistent, completed local evidence
// while honoring the caller's lifetime for the journal transaction.
func (o *Operation) ReadCompletedContext(ctx context.Context) (ResultRecord, CompletionRecord, error) {
	if o == nil || o.control == nil {
		return ResultRecord{}, CompletionRecord{}, os.ErrClosed
	}
	var result ResultRecord
	var completion CompletionRecord
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		if readErr := tx.Read(resultRecordName, &result); readErr != nil {
			return readErr
		}
		if _, validationErr := validateInspectionResult(result, o.digest); validationErr != nil {
			return validationErr
		}
		if stopErr := rejectStoppedEligibility(tx, result.Reason); stopErr != nil {
			return stopErr
		}
		if readErr := tx.Read(completionRecordName, &completion); readErr != nil {
			return readErr
		}
		if validationErr := o.validateCompletion(completion, result); validationErr != nil {
			return validationErr
		}
		completed, parseErr := canonicalTimestamp("inspection completion", completion.CompletedAt)
		if parseErr != nil {
			return task.ErrEvidenceFault
		}
		return o.validateStarted(tx, completed)
	})
	if err != nil {
		return ResultRecord{}, CompletionRecord{}, err
	}
	return result, completion, nil
}
