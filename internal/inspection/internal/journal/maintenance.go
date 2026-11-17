package journal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// MaintenanceObservation is journal-derived evidence, never native closure.
// Lifetime's reader expires with its callback; Disposition alone grants nothing.
type MaintenanceObservation struct {
	Deadline       time.Time
	Supervisor     task.SupervisorRef
	Label          string
	Lifetime       taskdir.ControlLifetimeObservation
	HistoricalExit bool
	Disposition    *contract.MaintenanceDispositionRecord
	Permission     bool
	// SettlementEligible is derived by the journal's preparation validator and
	// current-boot native observations; it is never permission or closure.
	SettlementEligible bool
	// RecoveryPending identifies valid tracked work for the canonical recovery
	// owner. It never authorizes settlement, process closure or maintenance.
	RecoveryPending bool
	canonicalClosed bool
}

type inspectionMaintenanceReader struct {
	operation    *Operation
	pin          taskdir.RootPin
	ownerPresent bool
	verified     *contract.MaintenanceDispositionSubject
	eligible     *contract.MaintenanceDispositionSubject
	recovery     *maintenanceRecoverySnapshot
}

type maintenanceRecoverySnapshot struct {
	subject      contract.MaintenanceDispositionSubject
	previousBoot bool
}

func openMaintenanceOperation(ctx context.Context, store *taskdir.Store, id string) (*Operation, error) {
	control, err := store.TryOpenInspectionContext(ctx, id)
	if err != nil {
		return nil, err
	}
	op := &Operation{store: store, control: control}
	err = control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		op.request, op.digest, op.deadline, readErr = readOperationRequestTx(ctx, tx, store, id)
		return readErr
	})
	if err != nil {
		return nil, errors.Join(err, control.CloseWithError(err))
	}
	return op, nil
}

func ObserveMaintenanceContext(ctx context.Context, store *taskdir.Store, id string, queue contract.MaintenanceQueue, native taskdir.OperatorDispositionNative, observe func(MaintenanceObservation) error) (resultErr error) {
	if store == nil || observe == nil {
		return task.ErrInvalidPermit
	}
	op, err := openMaintenanceOperation(ctx, store, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = op.control.CloseWithError(resultErr) }()
	owner, present, err := op.control.TryAcquireExistingLifetimeOwnerContext(ctx)
	if owner != nil {
		defer func() { resultErr = owner.ReleaseWithError(resultErr) }()
	}
	if err != nil {
		return err
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	reader := inspectionMaintenanceReader{operation: op, pin: pin, ownerPresent: present}
	if native != nil {
		if err = reader.verifyMaintenanceState(ctx, native); err != nil {
			return err
		}
	}
	if err = reader.verifyQueue(ctx, queue, native); err != nil {
		return err
	}
	return op.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		observation, readErr := reader.observeTx(ctx, tx)
		if readErr != nil {
			return readErr
		}
		return observe(observation)
	})
}

func (r *inspectionMaintenanceReader) verifyQueue(ctx context.Context, queue contract.MaintenanceQueue, native taskdir.OperatorDispositionNative) error {
	if queue.State == "" {
		return nil
	}
	if native == nil {
		return task.ErrInvalidPermit
	}
	if r.eligible == nil {
		return nil
	}
	if err := r.operation.requireMaintenanceQueue(ctx, queue); err != nil {
		return err
	}
	r.verified = r.eligible
	return nil
}

func (r *inspectionMaintenanceReader) verifyMaintenanceState(ctx context.Context, native taskdir.OperatorDispositionNative) error {
	var subject *contract.MaintenanceDispositionSubject
	var evidence task.LifetimeEvidence
	err := r.operation.control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		observation, readErr := r.observeFactsTx(ctx, tx)
		if readErr != nil || observation.canonicalClosed || observation.HistoricalExit || observation.Lifetime.PreparationOnly {
			return readErr
		}
		candidate, observed, readErr := r.operation.maintenanceSnapshotTx(ctx, tx, r.pin, r.ownerPresent)
		if readErr == nil {
			subject, evidence = &candidate, observed
		}
		return readErr
	})
	if err != nil || subject == nil {
		return err
	}
	recovery, err := maintenanceRecoveryFor(*subject, evidence, native)
	if err != nil {
		return err
	}
	if recovery != nil {
		r.recovery = recovery
		return nil
	}
	if err = requireCooperativeInspectionEvidence(subject.OperationID, evidence); err != nil {
		return err
	}
	if err = requireMaintenanceNativeEvidence(ctx, *subject, evidence, native); err != nil {
		return err
	}
	r.eligible = subject
	return nil
}

func maintenanceRecoveryFor(subject contract.MaintenanceDispositionSubject, evidence task.LifetimeEvidence, native taskdir.OperatorDispositionNative) (*maintenanceRecoverySnapshot, error) {
	boot, err := native.CurrentBootID()
	if err != nil {
		return nil, inspectionMaintenanceFact(subject.OperationID, "current_boot_identity", "native_observation_failed", err)
	}
	if !native.SameBoot(subject.BootID, boot) {
		if lifetime.CompareBootIDs(subject.BootID, boot) == lifetime.BootDifferent {
			return &maintenanceRecoverySnapshot{subject: subject, previousBoot: true}, nil
		}
		return nil, inspectionMaintenanceFact(subject.OperationID, "recorded_boot_identity", "inspection_boot_unobservable", task.ErrEvidenceFault)
	}
	// The domain reader validated identity links and scope/assurance pairs.
	// A valid partial transition or exact scope belongs to canonical recovery.
	if evidence.Registration == nil || evidence.Worker == nil || evidence.Registration.Assurance == task.LifetimeAssuranceExact {
		return &maintenanceRecoverySnapshot{subject: subject}, nil
	}
	return nil, nil
}

func (r inspectionMaintenanceReader) observeTx(ctx context.Context, tx *taskdir.ControlTransaction) (observation MaintenanceObservation, resultErr error) {
	observation, err := r.observeFactsTx(ctx, tx)
	if err != nil || observation.canonicalClosed || observation.HistoricalExit {
		return observation, err
	}
	if err = r.revalidateEligibilityTx(ctx, tx); err != nil {
		return observation, err
	}
	observation.RecoveryPending = r.recovery != nil
	// A positive previous-boot relation belongs to canonical recovery. An old
	// optional permission (including its physical pin) has no authority here.
	if r.recovery != nil && r.recovery.previousBoot {
		return observation, nil
	}
	observation.Disposition, err = r.readDispositionTx(ctx, tx)
	if err != nil {
		return observation, err
	}
	observation.SettlementEligible = r.eligible != nil
	observation.Permission = observation.Disposition != nil && r.verified != nil
	return observation, nil
}

func (r inspectionMaintenanceReader) observeFactsTx(ctx context.Context, tx *taskdir.ControlTransaction) (observation MaintenanceObservation, resultErr error) {
	o := r.operation
	request, digest, deadline, err := readOperationRequestTx(ctx, tx, o.store, o.request.IdentityID())
	if err != nil {
		return observation, err
	}
	lifetime, err := taskdir.NewControlLifetime(o.control, o.store.RootID, request.IdentityID(), task.LifetimeOwnerInspection, digest, deadline)
	if err != nil {
		return observation, err
	}
	records, err := lifetime.TransactionReader(tx)
	if err != nil {
		return observation, err
	}
	evidence, err := readMaintenanceLifetimeEvidence(ctx, request.IdentityID(), records)
	if err != nil {
		return observation, err
	}
	historicalExit, err := o.maintenanceHistoricalExitTx(tx, request, evidence)
	if err != nil {
		return observation, err
	}
	preparation, err := inspectionPreparationOnlyTx(tx, request, digest, deadline)
	if err != nil {
		return observation, err
	}
	observation = MaintenanceObservation{Deadline: deadline, Supervisor: request.Binding.Supervisor, Label: request.Label, Lifetime: taskdir.ControlLifetimeObservation{Records: records, OwnerPresent: r.ownerPresent, PreparationOnly: preparation}, HistoricalExit: historicalExit}
	proof, err := guardian.ReadAuxiliaryClosureProof(ctx, records)
	if err != nil && !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, guardian.ErrProofPending) }) {
		return observation, err
	}
	observation.canonicalClosed = proof.Closed()
	return observation, nil
}

func (r inspectionMaintenanceReader) revalidateEligibilityTx(ctx context.Context, tx *taskdir.ControlTransaction) error {
	expected := r.eligible
	if expected == nil && r.recovery != nil {
		expected = &r.recovery.subject
	}
	if expected == nil {
		return nil
	}
	subject, _, err := r.operation.maintenanceSnapshotTx(ctx, tx, r.pin, r.ownerPresent)
	if err != nil {
		return err
	}
	if subject != *expected {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (r inspectionMaintenanceReader) readDispositionTx(ctx context.Context, tx *taskdir.ControlTransaction) (*contract.MaintenanceDispositionRecord, error) {
	record, err := optionalMaintenanceRecord[contract.MaintenanceDispositionRecord](tx, maintenanceDispositionName)
	if err != nil || record == nil {
		return nil, err
	}
	if err = tx.Verify(maintenanceDispositionName, *record); err != nil {
		return nil, err
	}
	if err = contract.ValidateMaintenanceDisposition(record); err != nil {
		return nil, err
	}
	subject, _, err := r.operation.maintenanceSubjectTx(ctx, tx, r.pin, r.ownerPresent)
	if err != nil {
		return nil, err
	}
	if record.Subject != subject || (r.verified != nil && *r.verified != subject) {
		return nil, fmt.Errorf("%w: inspection settlement evidence changed", task.ErrIdentityMismatch)
	}
	return record, nil
}

func (o *Operation) maintenanceHistoricalExitTx(tx *taskdir.ControlTransaction, request contract.RequestRecord, evidence task.LifetimeEvidence) (bool, error) {
	if request.Binding.WorkerExitProof != "" || evidence.Intent != nil || evidence.Registration != nil {
		return false, nil
	}
	return o.historicalMaintenanceExitTx(tx)
}

func optionalMaintenanceRecord[T any](tx *taskdir.ControlTransaction, name string) (*T, error) {
	var record T
	err := tx.Read(name, &record)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspection evidence %s: %w", name, err)
	}
	return &record, nil
}

func readMaintenanceLifetimeEvidence(ctx context.Context, id string, records taskdir.LifetimeRecordReader) (task.LifetimeEvidence, error) {
	evidence, err := taskdir.LoadLifetimeEvidenceContext(ctx, records)
	if err != nil {
		return evidence, inspectionMaintenanceFact(id, "lifetime_identity_bindings", "invalid_evidence_record", err)
	}
	return evidence, nil
}

func (o *Operation) historicalWorkerObservationTx(tx *taskdir.ControlTransaction) (*contract.WorkerObservationRecord, error) {
	observation, err := optionalMaintenanceRecord[contract.WorkerObservationRecord](tx, workerObservationRecordName)
	if err != nil || observation == nil {
		return nil, err
	}
	var receipt contract.ReceiptRecord
	if err = tx.Read(receiptRecordName, &receipt); err != nil {
		return nil, fmt.Errorf("inspection evidence %s: %w", receiptRecordName, err)
	}
	if receipt.SchemaVersion != task.SchemaVersion || receipt.RequestSHA256 != o.digest || receipt.NumericTaskID == nil {
		return nil, task.ErrEvidenceFault
	}
	if observation.SchemaVersion != task.SchemaVersion || observation.RequestSHA256 != o.digest || observation.NumericTaskID < 0 || observation.NumericTaskID != *receipt.NumericTaskID {
		return nil, task.ErrEvidenceFault
	}
	if observation.State != "succeeded" && observation.State != "failed" {
		return nil, task.ErrEvidenceFault
	}
	return observation, nil
}

func (o *Operation) maintenanceCompletionTx(tx *taskdir.ControlTransaction) (*contract.ResultRecord, *contract.CompletionRecord, error) {
	result, err := optionalMaintenanceRecord[contract.ResultRecord](tx, resultRecordName)
	if err != nil {
		return nil, nil, err
	}
	if result != nil {
		if _, err = validateInspectionResult(*result, o.digest, o.factsLimit()); err != nil {
			return nil, nil, inspectionMaintenanceFact(o.request.IdentityID(), resultRecordName, "invalid_evidence_record", err)
		}
	}
	completion, err := optionalMaintenanceRecord[contract.CompletionRecord](tx, completionRecordName)
	if err != nil {
		return nil, nil, err
	}
	if completion == nil {
		return result, nil, nil
	}
	if result == nil {
		return nil, nil, inspectionMaintenanceFact(o.request.IdentityID(), resultRecordName, "missing_evidence_record", task.ErrEvidenceFault)
	}
	if err = o.validateCompletion(*completion, *result); err != nil {
		return nil, nil, inspectionMaintenanceFact(o.request.IdentityID(), completionRecordName, "invalid_evidence_record", err)
	}
	completed, err := contract.CanonicalTimestamp("inspection completion", completion.CompletedAt)
	if err != nil {
		return nil, nil, err
	}
	if err = o.validateStarted(tx, completed); err != nil {
		return nil, nil, err
	}
	return result, completion, nil
}

// Historical maintenance accepts independent worker exit, never admission proof.
// Failed workers may retain a valid result-only partial write.
func (o *Operation) historicalMaintenanceExitTx(tx *taskdir.ControlTransaction) (bool, error) {
	observation, err := o.historicalWorkerObservationTx(tx)
	if err != nil || observation == nil {
		return false, err
	}
	result, completion, err := o.maintenanceCompletionTx(tx)
	if err != nil {
		return false, err
	}
	if observation.CompletionSHA256 == "" {
		if observation.State != "failed" {
			return false, task.ErrEvidenceFault
		}
		return true, nil
	}
	if err = task.ValidateSHA256(observation.CompletionSHA256); err != nil {
		return false, err
	}
	if result == nil || completion == nil {
		return false, fmt.Errorf("%w: claimed inspection completion is missing", task.ErrEvidenceFault)
	}
	data, err := task.MarshalCanonical(completion)
	if err != nil {
		return false, err
	}
	if task.ComputeSHA256(data) != observation.CompletionSHA256 {
		return false, task.ErrEvidenceFault
	}
	if observation.State == "succeeded" && result.Reason != contract.ResultEligible {
		return false, task.ErrEvidenceFault
	}
	return true, nil
}
