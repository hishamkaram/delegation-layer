package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	receiptRecordName    = "receipt.json"
	resultRecordName     = "result.json"
	completionRecordName = "completion.json"
)

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
	record := contract.ReceiptRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, NumericTaskID: &id}
	records, err := o.controlLifetime(true)
	if err != nil {
		return err
	}
	return records.WithLifetimeLaunchPermissionOrReplayContext(ctx, func(tx *taskdir.ControlTransaction) error {
		var existing contract.ReceiptRecord
		if readErr := tx.Read(receiptRecordName, &existing); readErr != nil {
			return readErr
		}
		return tx.Verify(receiptRecordName, record)
	}, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(receiptRecordName, record)
		return putErr
	})
}

// AuthorizeManagedWorkerUpgradeContext records that queue migration verified
// the saved worker as a managed executable. The immutable request remains the
// authority for the original worker identity.
func (o *Operation) AuthorizeManagedWorkerUpgradeContext(ctx context.Context) error {
	if o == nil || o.control == nil {
		return task.ErrEvidenceFault
	}
	if ctx == nil {
		return context.Canceled
	}
	if o.request.Binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return task.ErrEvidenceFault
	}
	record := contract.ManagedWorkerUpgradeRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest}
	_, err := o.control.PutContext(ctx, workerUpgradeName, record)
	return err
}

// ManagedWorkerUpgradeAuthorizedContext reports whether queue migration
// durably authorized managed successors for this exact immutable request.
func (o *Operation) ManagedWorkerUpgradeAuthorizedContext(ctx context.Context) (bool, error) {
	if o == nil || o.control == nil {
		return false, os.ErrClosed
	}
	if ctx == nil {
		return false, context.Canceled
	}
	if o.request.Binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return false, task.ErrEvidenceFault
	}
	var record contract.ManagedWorkerUpgradeRecord
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(workerUpgradeName, &record)
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	if err != nil || record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != o.digest {
		return false, task.ErrEvidenceFault
	}
	return true, nil
}

func (o *Operation) Receipt() (contract.ReceiptRecord, error) {
	return o.ReceiptContext(context.Background())
}

// ReceiptContext reads the saved numeric target within the caller's lifetime.
func (o *Operation) ReceiptContext(ctx context.Context) (contract.ReceiptRecord, error) {
	if o == nil || o.control == nil {
		return contract.ReceiptRecord{}, os.ErrClosed
	}
	var record contract.ReceiptRecord
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(receiptRecordName, &record)
	})
	if err != nil {
		return contract.ReceiptRecord{}, classifyReceiptReadError(err)
	}
	if err := validateReceiptRecord(record, o.digest); err != nil {
		return contract.ReceiptRecord{}, task.ErrEvidenceFault
	}
	return record, nil
}

func classifyReceiptReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return task.ErrEvidenceFault
}

func validateReceiptRecord(record contract.ReceiptRecord, digest string) error {
	if record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != digest || record.NumericTaskID == nil || *record.NumericTaskID < 0 {
		return task.ErrEvidenceFault
	}
	return nil
}

// Complete persists result before completion using the shared create-once
// journal primitive. A failed completion write leaves an ineligible payload.
// The worker invokes this only after joining native and network work.
func (o *Operation) Complete(reason contract.ResultReason, facts json.RawMessage, now time.Time) error {
	return o.CompleteContext(context.Background(), reason, facts, now)
}

// CompleteContext persists the result and completion under the caller's
// authoritative lifetime. Replaying the same completion safely repairs a
// result whose completion record was not published by an earlier attempt.
func (o *Operation) CompleteContext(ctx context.Context, reason contract.ResultReason, facts json.RawMessage, now time.Time) error {
	if o == nil || o.control == nil || now.IsZero() {
		return task.ErrEvidenceFault
	}
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := newResultRecord(reason, facts, o.digest, o.factsLimit())
	if err != nil {
		return err
	}
	if reason == contract.ResultEligible && o.Expired(now) {
		return contract.ErrAdmissionExpired
	}
	resultPublished, err := o.completePreparedContext(ctx, reason, result, now)
	if err == nil || !resultPublished {
		return err
	}
	// Once result.json is durable, this operation owns resolving its paired
	// completion record. A caller deadline that expires between the two files
	// must not strand an otherwise valid result forever. The replay remains
	// create-once and rechecks start/stop authority; taskdir supplies the one
	// bounded persistence fallback for this recovery attempt.
	_, repairErr := o.completePreparedContext(context.WithoutCancel(ctx), reason, result, now)
	if repairErr == nil {
		return nil
	}
	return errors.Join(err, repairErr)
}

func (o *Operation) completePreparedContext(ctx context.Context, reason contract.ResultReason, result contract.ResultRecord, now time.Time) (resultPublished bool, resultErr error) {
	resultErr = withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
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
		completion := contract.CompletionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, ResultSHA256: task.ComputeSHA256(resultBytes), CompletedAt: contract.CanonicalNow(now), NativeExit: nativeExitClassification(reason)}
		var existing contract.CompletionRecord
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
		resultPublished = true
		_, putErr := tx.Put(completionRecordName, completion)
		return putErr
	})
	return resultPublished, resultErr
}

func rejectStoppedEligibility(tx *taskdir.ControlTransaction, reason contract.ResultReason) error {
	if reason != contract.ResultEligible {
		return nil
	}
	var stop contract.StopRequestRecord
	err := tx.Read(stopRequestRecordName, &stop)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return task.ErrEvidenceFault
	}
	return contract.ErrAdmissionExpired
}

func newResultRecord(reason contract.ResultReason, facts json.RawMessage, digest string, limits ...int) (contract.ResultRecord, error) {
	result := contract.ResultRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: digest, Reason: reason, Facts: map[string]json.RawMessage{}}
	if reason == contract.ResultEligible || len(facts) != 0 {
		if task.ValidateJSONStructure(facts) != nil || json.Unmarshal(facts, &result.Facts) != nil || result.Facts == nil {
			return contract.ResultRecord{}, task.ErrEvidenceFault
		}
	}
	if _, err := validateInspectionResult(result, digest, limits...); err != nil {
		return contract.ResultRecord{}, err
	}
	return result, nil
}

func (o *Operation) validateStarted(tx *taskdir.ControlTransaction, completed time.Time) error {
	var start contract.StartRecord
	if err := tx.Read(startRecordName, &start); err != nil {
		return err
	}
	if err := validateClaim(start); err != nil || start.RequestSHA256 != o.digest {
		return task.ErrEvidenceFault
	}
	started, err := contract.CanonicalTimestamp("inspection start", start.CreatedAt)
	if err != nil || completed.Before(started) || !started.Before(o.deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateInspectionResult(record contract.ResultRecord, digest string, limits ...int) (json.RawMessage, error) {
	if record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != digest {
		return nil, task.ErrEvidenceFault
	}
	switch record.Reason {
	case contract.ResultEligible:
		data, err := task.MarshalCanonical(record.Facts)
		if err != nil {
			return nil, task.ErrEvidenceFault
		}
		facts, err := contract.CanonicalFactsWithin(data, contract.InspectionFactsLimit(limits))
		if err != nil {
			return nil, task.ErrEvidenceFault
		}
		return facts, nil
	case contract.ResultUnavailable, contract.ResultExpired:
		if len(record.Facts) != 0 {
			return nil, task.ErrEvidenceFault
		}
		return nil, nil
	default:
		return nil, task.ErrEvidenceFault
	}
}

func (o *Operation) validateCompletion(completion contract.CompletionRecord, result contract.ResultRecord) error {
	if completion.SchemaVersion != task.SchemaVersion || completion.RequestSHA256 != o.digest || completion.NativeExit != nativeExitClassification(result.Reason) {
		return task.ErrEvidenceFault
	}
	data, err := task.MarshalCanonical(result)
	if err != nil || completion.ResultSHA256 != task.ComputeSHA256(data) {
		return task.ErrEvidenceFault
	}
	completed, err := contract.CanonicalTimestamp("inspection completion", completion.CompletedAt)
	if err != nil {
		return task.ErrEvidenceFault
	}
	created, err := contract.CanonicalTimestamp("inspection creation", o.request.CreatedAt)
	if err != nil || completed.Before(created) {
		return task.ErrEvidenceFault
	}
	if result.Reason == contract.ResultEligible && !completed.Before(o.deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func nativeExitClassification(reason contract.ResultReason) string {
	if reason == contract.ResultEligible {
		return "successful-exit"
	}
	return "unavailable"
}

// ReadCompleted returns only internally consistent, completed local evidence.
// The caller must separately observe a matching successful supervisor job
// before treating eligible facts as admission proof. This method never launches.
func (o *Operation) ReadCompleted() (contract.ResultRecord, contract.CompletionRecord, error) {
	return o.ReadCompletedContext(context.Background())
}

// ReadCompletedContext returns internally consistent, completed local evidence
// while honoring the caller's lifetime for the journal transaction.
func (o *Operation) ReadCompletedContext(ctx context.Context) (contract.ResultRecord, contract.CompletionRecord, error) {
	if o == nil || o.control == nil {
		return contract.ResultRecord{}, contract.CompletionRecord{}, os.ErrClosed
	}
	var result contract.ResultRecord
	var completion contract.CompletionRecord
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		if readErr := tx.Read(resultRecordName, &result); readErr != nil {
			return readErr
		}
		if _, validationErr := validateInspectionResult(result, o.digest, o.factsLimit()); validationErr != nil {
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
		completed, parseErr := contract.CanonicalTimestamp("inspection completion", completion.CompletedAt)
		if parseErr != nil {
			return task.ErrEvidenceFault
		}
		return o.validateStarted(tx, completed)
	})
	if err != nil {
		return contract.ResultRecord{}, contract.CompletionRecord{}, err
	}
	return result, completion, nil
}
