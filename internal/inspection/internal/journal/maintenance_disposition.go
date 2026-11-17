package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const maintenanceDispositionName = "operator-disposition.json"

// withMaintenanceOwner uses only an existing inode. Creating an exclusion
// would not establish that the original guardian has released its authority.
func (o *Operation) withMaintenanceOwner(ctx context.Context, action func() error) (resultErr error) {
	if o == nil || o.control == nil || ctx == nil {
		return task.ErrInvalidPermit
	}
	owner, present, err := o.control.TryAcquireExistingLifetimeOwnerContext(ctx)
	if owner != nil {
		defer func() { resultErr = owner.ReleaseWithError(resultErr) }()
	}
	if err != nil {
		return err
	}
	if !present {
		return inspectionMaintenanceFact(o.request.IdentityID(), ".lifetime-owner.lock", "missing_evidence_record", task.ErrEvidenceFault)
	}
	return action()
}

func (o *Operation) maintenanceSubjectTx(ctx context.Context, tx *taskdir.ControlTransaction, pin taskdir.RootPin, ownerPresent bool) (subject contract.MaintenanceDispositionSubject, evidence task.LifetimeEvidence, resultErr error) {
	subject, evidence, resultErr = o.maintenanceSnapshotTx(ctx, tx, pin, ownerPresent)
	if resultErr != nil {
		return subject, evidence, resultErr
	}
	return subject, evidence, requireCooperativeInspectionEvidence(o.request.IdentityID(), evidence)
}

// maintenanceSnapshotTx validates the shared evidence and immutable inventory.
// Recovery and settlement use this same snapshot; settlement adds its narrower
// cooperative-worker and current-native-absence requirements.
func (o *Operation) maintenanceSnapshotTx(ctx context.Context, tx *taskdir.ControlTransaction, pin taskdir.RootPin, ownerPresent bool) (subject contract.MaintenanceDispositionSubject, evidence task.LifetimeEvidence, resultErr error) {
	if !ownerPresent {
		return subject, evidence, inspectionMaintenanceFact(o.request.IdentityID(), ".lifetime-owner.lock", "missing_evidence_record", task.ErrEvidenceFault)
	}
	if err := o.validateMaintenanceSubjectRootTx(ctx, tx, pin); err != nil {
		return subject, evidence, err
	}
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return subject, evidence, err
	}
	records, err := lifetime.TransactionReader(tx)
	if err != nil {
		return subject, evidence, err
	}
	evidence, err = readMaintenanceLifetimeEvidence(ctx, o.request.IdentityID(), records)
	if err != nil {
		return subject, evidence, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return subject, evidence, err
	}
	if closure.Closed() {
		return subject, evidence, fmt.Errorf("%w: canonical inspection closure already exists", task.ErrTerminalTask)
	}
	if evidence.Intent == nil {
		return subject, evidence, inspectionMaintenanceFact(o.request.IdentityID(), inspectionLifetimeIntentName, "missing_evidence_record", task.ErrEvidenceFault)
	}
	if _, _, err = o.maintenanceCompletionTx(tx); err != nil {
		return subject, evidence, err
	}
	if err = o.validateMaintenanceClaimsTx(tx); err != nil {
		return subject, evidence, err
	}
	digest, err := o.maintenanceInventoryTx(tx)
	if err != nil {
		return subject, evidence, err
	}
	subject = contract.MaintenanceDispositionSubject{RootID: pin.RootID(), RootDevice: pin.Device(), RootInode: pin.Inode(), OperationID: o.request.IdentityID(), RequestSHA256: o.digest, EvidenceSHA256: digest, BootID: evidence.Intent.BootID, Permission: task.OperatorPermissionSupervisorMaintenance}
	return subject, evidence, nil
}

type maintenanceRecordDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func (o *Operation) maintenanceInventoryTx(tx *taskdir.ControlTransaction) (string, error) {
	names, err := tx.RecordNames()
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	inventory := make([]maintenanceRecordDigest, 0, len(names))
	for _, name := range names {
		if name == ".operation.lock" || name == ".lifetime-owner.lock" || name == maintenanceDispositionName {
			continue
		}
		if taskdir.IsRecognizedStageFile(name) {
			if err = o.verifyDispositionStageTx(tx, name); err != nil {
				return "", err
			}
			continue
		}
		if !maintenanceRecordKnown(name) {
			return "", inspectionMaintenanceFact(o.request.IdentityID(), name, "unclassified_evidence_record", task.ErrEvidenceFault)
		}
		var record json.RawMessage
		if err = tx.Read(name, &record); err != nil {
			return "", err
		}
		if err = tx.Verify(name, record); err != nil {
			return "", err
		}
		data, marshalErr := task.MarshalCanonical(record)
		if marshalErr != nil {
			return "", marshalErr
		}
		inventory = append(inventory, maintenanceRecordDigest{Name: name, SHA256: task.ComputeSHA256(data)})
	}
	data, err := task.MarshalCanonical(inventory)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func requireCooperativeInspectionEvidence(id string, evidence task.LifetimeEvidence) error {
	reg, worker := evidence.Registration, evidence.Worker
	for _, missing := range []struct {
		name   string
		absent bool
	}{{inspectionLifetimeIntentName, evidence.Intent == nil}, {inspectionLifetimeRegistrationName, reg == nil}, {inspectionLifetimeWorkerName, worker == nil}} {
		if missing.absent {
			return inspectionMaintenanceFact(id, missing.name, "missing_evidence_record", task.ErrEvidenceFault)
		}
	}
	if reg.OwnerKind != task.LifetimeOwnerInspection || reg.Assurance != task.LifetimeAssuranceCooperative || reg.ScopeKind != task.LifetimeScopeProcessGroup {
		return inspectionMaintenanceFact(id, inspectionLifetimeRegistrationName, "inspection_settlement_unsupported_lifetime", task.ErrEvidenceFault)
	}
	return nil
}

func (o *Operation) requireMaintenanceQueue(ctx context.Context, queue contract.MaintenanceQueue) error {
	if queue.Label != o.request.Label {
		return task.ErrIdentityMismatch
	}
	switch queue.State {
	case contract.MaintenanceQueueAbsent:
		if queue.NumericTaskID != nil {
			return task.ErrIdentityMismatch
		}
	case contract.MaintenanceQueueEnded:
		receipt, err := o.ReceiptContext(ctx)
		if err != nil {
			return err
		}
		if receipt.NumericTaskID == nil || queue.NumericTaskID == nil || *receipt.NumericTaskID != *queue.NumericTaskID || *queue.NumericTaskID < 0 {
			return task.ErrIdentityMismatch
		}
	default:
		return task.ErrIdentityMismatch
	}
	return nil
}

func requireMaintenanceProcessAbsent(ctx context.Context, native taskdir.OperatorDispositionNative, pid int, start string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gone, err := native.ProcessAbsent(pid, start)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%w: recorded inspection process remains live", task.ErrLockBusy)
	}
	return nil
}

func (o *Operation) requireMaintenanceNative(ctx context.Context, subject contract.MaintenanceDispositionSubject, evidence task.LifetimeEvidence, queue contract.MaintenanceQueue, native taskdir.OperatorDispositionNative) error {
	if err := o.requireMaintenanceQueue(ctx, queue); err != nil {
		return err
	}
	return requireMaintenanceNativeEvidence(ctx, subject, evidence, native)
}

func requireMaintenanceNativeEvidence(ctx context.Context, subject contract.MaintenanceDispositionSubject, evidence task.LifetimeEvidence, native taskdir.OperatorDispositionNative) error {
	if native == nil {
		return task.ErrInvalidPermit
	}
	boot, err := native.CurrentBootID()
	if err != nil {
		return inspectionMaintenanceFact(subject.OperationID, "current_boot_identity", "native_observation_failed", err)
	}
	if !native.SameBoot(subject.BootID, boot) || evidence.Worker == nil || !native.SameBoot(evidence.Worker.BootID, boot) {
		return inspectionMaintenanceFact(subject.OperationID, "recorded_boot_identity", "inspection_settlement_requires_current_boot", task.ErrIdentityMismatch)
	}
	if err = requireMaintenanceProcessAbsent(ctx, native, evidence.Worker.PID, evidence.Worker.ProcessStart); err != nil {
		return inspectionMaintenanceNativeFact(subject.OperationID, inspectionLifetimeWorkerName, err)
	}
	for _, launch := range evidence.Launches {
		if err = requireMaintenanceProcessAbsent(ctx, native, launch.PID, launch.ProcessStart); err != nil {
			return inspectionMaintenanceNativeFact(subject.OperationID, inspectionLifetimeLaunchName(launch.Sequence), err)
		}
	}
	return ctx.Err()
}

func inspectionMaintenanceFact(id, name, reason string, cause error) error {
	var existing *task.ControlFactError
	if errors.As(cause, &existing) {
		return cause
	}
	return &task.ControlFactError{ResourceKind: "inspection", ResourceID: id, FactKind: "evidence", Name: name, ReasonCode: reason, NextAction: "stop_and_report", Cause: cause}
}

func inspectionMaintenanceNativeFact(id, name string, cause error) error {
	if task.AllErrorCauses(cause, func(err error) bool {
		return errors.Is(err, task.ErrLockBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}) {
		return cause
	}
	return inspectionMaintenanceFact(id, name, "native_observation_failed", cause)
}

// PrepareMaintenanceDispositionContext is read-only. Its caller retains the
// current supervisor generation exclusion and validates the queue mapping.
func (o *Operation) PrepareMaintenanceDispositionContext(ctx context.Context, pin taskdir.RootPin, queue contract.MaintenanceQueue, native taskdir.OperatorDispositionNative) (subject contract.MaintenanceDispositionSubject, challenge string, resultErr error) {
	resultErr = o.withMaintenanceOwner(ctx, func() error {
		var evidence task.LifetimeEvidence
		err := o.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
			var err error
			subject, evidence, err = o.maintenanceSubjectTx(ctx, tx, pin, true)
			return err
		})
		if err != nil {
			return err
		}
		if err = o.requireMaintenanceNative(ctx, subject, evidence, queue, native); err != nil {
			return err
		}
		challenge, err = contract.MaintenanceDispositionChallenge(subject)
		return err
	})
	return subject, challenge, resultErr
}

// ConfirmMaintenanceDispositionContext is the only disposition writer. A
// matching winner is historical evidence, never a current maintenance grant.
func (o *Operation) ConfirmMaintenanceDispositionContext(ctx context.Context, pin taskdir.RootPin, queue contract.MaintenanceQueue, challenge string, native taskdir.OperatorDispositionNative) (winner *contract.MaintenanceDispositionRecord, resultErr error) {
	if err := task.ValidateSHA256(challenge); err != nil {
		return nil, err
	}
	resultErr = o.withMaintenanceOwner(ctx, func() error {
		var err error
		winner, err = o.confirmMaintenanceOwned(ctx, pin, queue, challenge, native)
		return err
	})
	return winner, resultErr
}

func (o *Operation) confirmMaintenanceOwned(ctx context.Context, pin taskdir.RootPin, queue contract.MaintenanceQueue, challenge string, native taskdir.OperatorDispositionNative) (winner *contract.MaintenanceDispositionRecord, resultErr error) {
	var subject contract.MaintenanceDispositionSubject
	var evidence task.LifetimeEvidence
	err := o.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		winner, readErr = (inspectionMaintenanceReader{operation: o, pin: pin, ownerPresent: true}).readDispositionTx(ctx, tx)
		if readErr != nil {
			return readErr
		}
		if winner != nil {
			return requireMatchingMaintenanceChallenge(winner, challenge)
		}
		subject, evidence, readErr = o.maintenanceSubjectTx(ctx, tx, pin, true)
		return readErr
	})
	if err != nil || winner != nil {
		return winner, err
	}
	actual, err := contract.MaintenanceDispositionChallenge(subject)
	if err != nil {
		return nil, err
	}
	if actual != challenge {
		return nil, fmt.Errorf("%w: stale inspection settlement challenge", task.ErrIdentityMismatch)
	}
	if err = o.requireMaintenanceNative(ctx, subject, evidence, queue, native); err != nil {
		return nil, err
	}
	err = o.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		after, _, readErr := o.maintenanceSubjectTx(ctx, tx, pin, true)
		if readErr != nil {
			return readErr
		}
		if after != subject {
			return task.ErrIdentityMismatch
		}
		winner, readErr = commitMaintenanceDispositionTx(tx, subject, challenge)
		return readErr
	})
	return winner, err
}

func requireMatchingMaintenanceChallenge(record *contract.MaintenanceDispositionRecord, challenge string) error {
	if record.Challenge != challenge {
		return task.ErrIdentityMismatch
	}
	return nil
}

func commitMaintenanceDispositionTx(tx *taskdir.ControlTransaction, subject contract.MaintenanceDispositionSubject, challenge string) (*contract.MaintenanceDispositionRecord, error) {
	candidate := contract.MaintenanceDispositionRecord{SchemaVersion: task.SchemaVersion, Subject: subject, Challenge: challenge, ConfirmedAt: contract.CanonicalNow(time.Now())}
	_, writeErr := tx.Put(maintenanceDispositionName, candidate)
	var record contract.MaintenanceDispositionRecord
	readErr := tx.Read(maintenanceDispositionName, &record)
	if task.RecordAbsent(readErr) {
		// This authoritative absence permits one replay of this exact candidate.
		_, retryErr := tx.Put(maintenanceDispositionName, candidate)
		writeErr = errors.Join(writeErr, retryErr)
		readErr = tx.Read(maintenanceDispositionName, &record)
	}
	if readErr != nil {
		return nil, errors.Join(writeErr, readErr)
	}
	if err := contract.ValidateMaintenanceDisposition(&record); err != nil {
		return nil, errors.Join(writeErr, err)
	}
	if record != candidate {
		return nil, errors.Join(writeErr, task.ErrInvariantFault)
	}
	return &record, writeErr
}

// ReadMaintenanceDispositionContext validates immutable evidence only. A caller
// must still observe the current queue and native identities before maintenance.
func (o *Operation) ReadMaintenanceDispositionContext(ctx context.Context, pin taskdir.RootPin) (record *contract.MaintenanceDispositionRecord, resultErr error) {
	resultErr = o.withMaintenanceOwner(ctx, func() error {
		return o.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
			var saved contract.MaintenanceDispositionRecord
			if err := tx.Read(maintenanceDispositionName, &saved); err != nil {
				return err
			}
			if err := contract.ValidateMaintenanceDisposition(&saved); err != nil {
				return err
			}
			subject, _, err := o.maintenanceSubjectTx(ctx, tx, pin, true)
			if err != nil {
				return err
			}
			if saved.Subject != subject {
				return fmt.Errorf("%w: inspection settlement evidence changed", task.ErrIdentityMismatch)
			}
			record = &saved
			return nil
		})
	})
	return record, resultErr
}

func maintenanceRecordKnown(name string) bool {
	switch name {
	case requestRecordName, submissionRecordName, startRecordName, receiptRecordName, resultRecordName, completionRecordName, workerObservationRecordName, workerUpgradeName, stopRequestRecordName, stopReplyRecordName, stopObservationRecordName, inspectionLifetimeIntentName, inspectionLifetimeRegistrationName, inspectionLifetimeWorkerName, inspectionLifetimeWorkerWaitName, inspectionLifetimeTerminationName, "lifetime-unlaunched.json":
		return true
	default:
		return strings.HasPrefix(name, inspectionLifetimeLaunchPrefix) && strings.HasSuffix(name, inspectionLifetimeLaunchSuffix)
	}
}

func (o *Operation) validateMaintenanceClaimsTx(tx *taskdir.ControlTransaction) error {
	for _, name := range []string{submissionRecordName, startRecordName} {
		record, err := optionalMaintenanceRecord[contract.SubmissionRecord](tx, name)
		if err != nil {
			return err
		}
		if record == nil {
			continue
		}
		if err = validateClaim(*record); err != nil {
			return inspectionMaintenanceFact(o.request.IdentityID(), name, "invalid_evidence_record", err)
		}
		if record.RequestSHA256 != o.digest {
			return inspectionMaintenanceFact(o.request.IdentityID(), name, "invalid_evidence_record", task.ErrIdentityMismatch)
		}
	}
	receipt, err := optionalMaintenanceRecord[contract.ReceiptRecord](tx, receiptRecordName)
	if err != nil {
		return err
	}
	if receipt != nil && (receipt.SchemaVersion != task.SchemaVersion || receipt.RequestSHA256 != o.digest || receipt.NumericTaskID == nil || *receipt.NumericTaskID < 0) {
		return inspectionMaintenanceFact(o.request.IdentityID(), receiptRecordName, "invalid_evidence_record", task.ErrEvidenceFault)
	}
	return nil
}

func (o *Operation) validateMaintenanceSubjectRootTx(ctx context.Context, tx *taskdir.ControlTransaction, pin taskdir.RootPin) error {
	if o.store == nil || pin.RootID() != o.request.RootID {
		return task.ErrIdentityMismatch
	}
	current, err := o.store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(pin) {
		return task.ErrIdentityMismatch
	}
	_, digest, _, err := readOperationRequestTx(ctx, tx, o.store, o.request.IdentityID())
	if err != nil {
		return err
	}
	if digest != o.digest {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (o *Operation) verifyDispositionStageTx(tx *taskdir.ControlTransaction, name string) error {
	record, err := optionalMaintenanceRecord[contract.MaintenanceDispositionRecord](tx, maintenanceDispositionName)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("%w: uncommitted inspection stage %s", task.ErrEvidenceFault, name)
	}
	if err = contract.ValidateMaintenanceDisposition(record); err != nil {
		return err
	}
	return tx.VerifyStageRecord(name, maintenanceDispositionName, *record)
}
