package journal

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const (
	captureNamespacePrefix       = "capture-"
	captureCandidateRecordPrefix = "capture-candidate-"
	captureReceiptRecordPrefix   = "capture-receipt-"
)

// RecordCaptureCandidate publishes the immutable Git candidate before the
// coordinator creates its expected-absent tree ref.
func (r *Resource) RecordCaptureCandidate(ctx context.Context, candidate contract.CaptureCandidate) error {
	if err := validateCaptureCandidateRecord(candidate, r.request.WorkspaceID, candidate.OperationID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := candidateMatchesSourceTx(tx, candidate); err != nil {
			return err
		}
		if _, err := tx.Put(captureCandidateName(candidate.OperationID), candidate); err != nil {
			return err
		}
		return tx.Verify(captureCandidateName(candidate.OperationID), candidate)
	})
}

// CaptureCandidate reads one exact operation-bound candidate.
func (r *Resource) CaptureCandidate(ctx context.Context, operationID string) (candidate contract.CaptureCandidate, resultErr error) {
	if err := task.ValidateTaskID(operationID); err != nil {
		return candidate, err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		candidate, err = readCaptureCandidateTx(tx, r.request.WorkspaceID, operationID)
		if err != nil {
			return err
		}
		return classifyStoredResourceRecordError(candidateMatchesSourceTx(tx, candidate))
	})
	return candidate, resultErr
}

// RecordCaptureReceipt stores root-proven capture evidence. It never changes
// a candidate or a provider outcome and never starts a helper.
func (r *Resource) RecordCaptureReceipt(ctx context.Context, receipt contract.CaptureReceipt) error {
	if err := validateCaptureReceiptRecord(receipt, r.request.WorkspaceID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if receipt.Candidate != nil {
			candidate, err := readCaptureCandidateTx(tx, r.request.WorkspaceID, receipt.OperationID)
			if err != nil {
				if task.RecordAbsent(err) {
					return errors.Join(task.ErrEvidenceFault, err)
				}
				return err
			}
			if candidate != *receipt.Candidate {
				return task.ErrIdentityMismatch
			}
			if err := candidateMatchesSourceTx(tx, candidate); err != nil {
				return classifyStoredResourceRecordError(err)
			}
		}
		if _, err := tx.Put(captureReceiptName(receipt.OperationID), receipt); err != nil {
			return err
		}
		return tx.Verify(captureReceiptName(receipt.OperationID), receipt)
	})
}

// CaptureReceipt reads one exact root-proven capture receipt and validates its
// optional candidate echo against the immutable candidate record.
func (r *Resource) CaptureReceipt(ctx context.Context, operationID string) (receipt contract.CaptureReceipt, resultErr error) {
	if err := task.ValidateTaskID(operationID); err != nil {
		return receipt, err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		receipt, err = readCaptureReceiptTx(tx, r.request.WorkspaceID, operationID)
		return err
	})
	return receipt, resultErr
}

// CaptureIDs enumerates and validates every capture candidate and receipt;
// filename parsing only identifies which exact records must be decoded.
func (r *Resource) CaptureIDs(ctx context.Context) (ids []string, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		candidates, receipts, err := r.readCaptureIndex(ctx, tx)
		if err != nil {
			return err
		}
		if err = validateCaptureIndexSource(tx, candidates); err != nil {
			return err
		}
		ids = appendCaptureIDs(ids, candidates, receipts)
		return nil
	})
	if resultErr != nil {
		return nil, resultErr
	}
	slices.Sort(ids)
	return ids, nil
}

func (r *Resource) readCaptureIndex(ctx context.Context, tx *taskdir.ControlTransaction) (candidates map[string]contract.CaptureCandidate, receipts map[string]bool, resultErr error) {
	names, err := r.control.RecordNamesContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	candidates = make(map[string]contract.CaptureCandidate)
	receipts = make(map[string]bool)
	for _, name := range names {
		id, _, candidateRecord, candidateEntry, receiptEntry, parseErr := readCaptureEntry(tx, name, r.request.WorkspaceID)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		if !candidateEntry && !receiptEntry {
			continue
		}
		if candidateEntry {
			candidates[id] = candidateRecord
		} else {
			receipts[id] = true
		}
	}
	return candidates, receipts, nil
}

func readCaptureEntry(tx *taskdir.ControlTransaction, name, workspaceID string) (id, kind string, candidate contract.CaptureCandidate, candidateEntry, receiptEntry bool, resultErr error) {
	id, kind, family, err := parseCaptureRecordName(name)
	if err != nil || !family {
		return "", "", contract.CaptureCandidate{}, false, false, err
	}
	if kind == captureCandidateKind {
		candidate, err = readCaptureCandidateTx(tx, workspaceID, id)
		if err != nil {
			return "", "", contract.CaptureCandidate{}, false, false, requireResourceRecord(err)
		}
		return id, kind, candidate, true, false, nil
	}
	if _, err = readCaptureReceiptTx(tx, workspaceID, id); err != nil {
		return "", "", contract.CaptureCandidate{}, false, false, requireResourceRecord(err)
	}
	return id, kind, contract.CaptureCandidate{}, false, true, nil
}

func validateCaptureIndexSource(tx *taskdir.ControlTransaction, candidates map[string]contract.CaptureCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	source, err := readSourceTx(tx)
	if err != nil {
		if task.RecordAbsent(err) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	for _, candidate := range candidates {
		if candidate.BaseOID != source.BaseOID || candidate.ObjectFormat != source.ObjectFormat {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
	}
	return nil
}

func appendCaptureIDs(ids []string, candidates map[string]contract.CaptureCandidate, receipts map[string]bool) []string {
	for id := range candidates {
		ids = append(ids, id)
	}
	for id := range receipts {
		if _, present := candidates[id]; !present {
			ids = append(ids, id)
		}
	}
	return ids
}

const (
	captureCandidateKind = "candidate"
	captureReceiptKind   = "receipt"
)

func captureCandidateName(operationID string) string {
	return captureCandidateRecordPrefix + operationID + ".json"
}

func captureReceiptName(operationID string) string {
	return captureReceiptRecordPrefix + operationID + ".json"
}

func readCaptureCandidateTx(tx *taskdir.ControlTransaction, workspaceID, operationID string) (contract.CaptureCandidate, error) {
	if task.ValidateTaskID(operationID) != nil {
		return contract.CaptureCandidate{}, task.ErrIdentityMismatch
	}
	var candidate contract.CaptureCandidate
	if err := tx.Read(captureCandidateName(operationID), &candidate); err != nil {
		return contract.CaptureCandidate{}, classifyResourceRecordReadError(err)
	}
	if err := validateCaptureCandidateRecord(candidate, workspaceID, operationID); err != nil {
		return contract.CaptureCandidate{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(captureCandidateName(operationID), candidate); err != nil {
		return contract.CaptureCandidate{}, err
	}
	return candidate, nil
}

func readCaptureReceiptTx(tx *taskdir.ControlTransaction, workspaceID, operationID string) (contract.CaptureReceipt, error) {
	if task.ValidateTaskID(operationID) != nil {
		return contract.CaptureReceipt{}, task.ErrIdentityMismatch
	}
	var receipt contract.CaptureReceipt
	if err := tx.Read(captureReceiptName(operationID), &receipt); err != nil {
		return contract.CaptureReceipt{}, classifyResourceRecordReadError(err)
	}
	if err := validateCaptureReceiptRecord(receipt, workspaceID); err != nil {
		return contract.CaptureReceipt{}, classifyStoredResourceRecordError(err)
	}
	if receipt.OperationID != operationID {
		return contract.CaptureReceipt{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := tx.Verify(captureReceiptName(operationID), receipt); err != nil {
		return contract.CaptureReceipt{}, err
	}
	if receipt.Candidate != nil {
		candidate, err := readCaptureCandidateTx(tx, workspaceID, operationID)
		if err != nil {
			if task.RecordAbsent(err) {
				return contract.CaptureReceipt{}, errors.Join(task.ErrEvidenceFault, err)
			}
			return contract.CaptureReceipt{}, err
		}
		if candidate != *receipt.Candidate {
			return contract.CaptureReceipt{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		if err = candidateMatchesSourceTx(tx, candidate); err != nil {
			return contract.CaptureReceipt{}, classifyStoredResourceRecordError(err)
		}
	}
	return receipt, nil
}

func candidateMatchesSourceTx(tx *taskdir.ControlTransaction, candidate contract.CaptureCandidate) error {
	source, err := readSourceTx(tx)
	if err != nil {
		if task.RecordAbsent(err) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if candidate.BaseOID != source.BaseOID || candidate.ObjectFormat != source.ObjectFormat {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateCaptureCandidateRecord(candidate contract.CaptureCandidate, workspaceID, operationID string) error {
	if candidate.SchemaVersion != task.SchemaVersion || candidate.WorkspaceID != workspaceID || candidate.OperationID != operationID {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateTaskID(candidate.WorkspaceID), task.ValidateTaskID(candidate.OperationID), contract.ValidateSubject(candidate.Subject)); err != nil {
		return err
	}
	if err := contract.ValidateObjectFormat(candidate.ObjectFormat); err != nil {
		return err
	}
	for _, oid := range []string{candidate.BaseOID, candidate.ObservedHEAD, candidate.TreeOID} {
		if err := contract.ValidateOID(oid, candidate.ObjectFormat); err != nil {
			return err
		}
	}
	return contract.ValidateOwnedRef(candidate.SnapshotRef)
}

func validateCaptureReceiptRecord(receipt contract.CaptureReceipt, workspaceID string) error {
	if receipt.SchemaVersion != task.SchemaVersion || receipt.WorkspaceID != workspaceID {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateTaskID(receipt.WorkspaceID), task.ValidateTaskID(receipt.OperationID), contract.ValidateSubject(receipt.Subject), validateCaptureClosure(receipt)); err != nil {
		return err
	}
	switch receipt.Status {
	case "captured":
		if receipt.Candidate == nil {
			return task.ErrIdentityMismatch
		}
	case "unavailable":
	default:
		return task.ErrIdentityMismatch
	}
	if receipt.Candidate != nil {
		if err := validateCaptureCandidateRecord(*receipt.Candidate, workspaceID, receipt.OperationID); err != nil {
			return err
		}
		if receipt.Candidate.Subject != receipt.Subject {
			return task.ErrIdentityMismatch
		}
	}
	for _, artifact := range receipt.Artifacts {
		if err := validateArtifact(artifact); err != nil {
			return err
		}
		if artifact.Truncated {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}

func validateCaptureClosure(receipt contract.CaptureReceipt) error {
	switch receipt.LifetimeClosure {
	case task.LifetimeClosureUnregisteredRecovered:
		return validateIntentCaptureClosure(receipt)
	case task.LifetimeClosureLegacyPreviousBoot:
		return validatePreviousBootCaptureClosure(receipt)
	case task.LifetimeClosureOpen, task.LifetimeClosureRegisteredNative, task.LifetimeClosureRegisteredRecovered:
		return task.ErrInvalidEnum
	case "":
	default:
		return task.ErrInvalidEnum
	}
	if receipt.UnlaunchedSHA256 != "" {
		if err := validateUnavailableCaptureClosure(receipt, "capture_not_launched"); err != nil {
			return err
		}
		if receipt.TerminationSHA256 != "" || receipt.WorkerWaitSHA256 != "" {
			return task.ErrIdentityMismatch
		}
		return task.ValidateSHA256(receipt.UnlaunchedSHA256)
	}
	return errors.Join(task.ValidateSHA256(receipt.TerminationSHA256), task.ValidateSHA256(receipt.WorkerWaitSHA256))
}

func validateUnavailableCaptureClosure(receipt contract.CaptureReceipt, reason string) error {
	if receipt.Status != "unavailable" || receipt.ReasonCode != reason || receipt.Candidate != nil || len(receipt.Artifacts) != 0 {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateIntentCaptureClosure(receipt contract.CaptureReceipt) error {
	if err := validateUnavailableCaptureClosure(receipt, "capture_not_launched"); err != nil {
		return err
	}
	if receipt.TerminationSHA256 != "" || receipt.UnlaunchedSHA256 != "" {
		return task.ErrIdentityMismatch
	}
	return task.ValidateSHA256(receipt.WorkerWaitSHA256)
}

func validatePreviousBootCaptureClosure(receipt contract.CaptureReceipt) error {
	if receipt.WorkerWaitSHA256 != "" || receipt.UnlaunchedSHA256 != "" {
		return task.ErrIdentityMismatch
	}
	return task.ValidateSHA256(receipt.TerminationSHA256)
}

func validateArtifact(artifact contract.Artifact) error {
	return contract.ValidateArtifact(artifact)
}

func parseCaptureRecordName(name string) (string, string, bool, error) {
	switch {
	case strings.HasPrefix(name, captureCandidateRecordPrefix):
		id, candidate, err := validateIDName(name, captureCandidateRecordPrefix, ".json")
		return id, captureCandidateKind, candidate, err
	case strings.HasPrefix(name, captureReceiptRecordPrefix):
		id, candidate, err := validateIDName(name, captureReceiptRecordPrefix, ".json")
		return id, captureReceiptKind, candidate, err
	case strings.HasPrefix(name, capturePurposeRecordPrefix), strings.HasPrefix(name, captureObligationRecordPrefix):
		return "", "", false, nil
	case strings.HasPrefix(name, captureNamespacePrefix):
		return "", "", true, task.ErrEvidenceFault
	default:
		return "", "", false, nil
	}
}
