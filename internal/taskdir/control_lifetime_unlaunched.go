package taskdir

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const controlLifetimeUnlaunchedName = "lifetime-unlaunched.json"

var errMissingLifetimeLaunchPermissionCallback = errors.New("missing lifetime launch permission callback")

// ReadLifetimeUnlaunchedContext reads the auxiliary launch fence and
// validates that no native lifetime record contradicts it.
func (l *ControlLifetime) ReadLifetimeUnlaunchedContext(ctx context.Context) (result *task.LifetimeUnlaunchedRecord, resultErr error) {
	if err := validateControlLifetimeUnlaunched(l, false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeUnlaunchedTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SealLifetimeUnlaunchedContext creates the one auxiliary launch fence after
// the immutable operation deadline. The owner lease is acquired first and
// held through the canonical control transaction so a live guardian cannot
// race native intent publication.
func (l *ControlLifetime) SealLifetimeUnlaunchedContext(ctx context.Context) (result *task.LifetimeUnlaunchedRecord, resultErr error) {
	return l.sealLifetimeUnlaunchedContextWithNow(ctx, time.Now)
}

func (l *ControlLifetime) sealLifetimeUnlaunchedContextWithNow(ctx context.Context, now func() time.Time) (result *task.LifetimeUnlaunchedRecord, resultErr error) {
	if err := validateControlLifetimeUnlaunched(l, true); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, task.ErrInvalidPermit
	}
	owner, err := l.control.TryAcquireLifetimeOwnerContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()

	err = withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var transactionErr error
		result, transactionErr = sealControlLifetimeUnlaunchedTx(tx, l, now)
		return transactionErr
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// WithLifetimeLaunchPermissionContext gives a caller one canonical lock
// boundary for its receipt/start guard. The callback cannot run after either
// the unlaunched fence or native termination has become authoritative.
func (l *ControlLifetime) WithLifetimeLaunchPermissionContext(ctx context.Context, callback func(*ControlTransaction) error) error {
	return l.WithLifetimeLaunchPermissionOrReplayContext(ctx, nil, callback)
}

// WithLifetimeLaunchPermissionOrReplayContext permits exact readback of an
// existing immutable fact after closure. Only an absent fact may reach create,
// and create remains fenced by the shared native/unlaunched completion gate.
func (l *ControlLifetime) WithLifetimeLaunchPermissionOrReplayContext(ctx context.Context, replay, create func(*ControlTransaction) error) error {
	if err := validateControlLifetimeUnlaunched(l, true); err != nil {
		return err
	}
	if create == nil {
		return errMissingLifetimeLaunchPermissionCallback
	}
	return withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		if replay != nil {
			err := replay(tx)
			if err == nil || !task.RecordAbsent(err) {
				return err
			}
		}
		if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
			return err
		}
		return create(tx)
	})
}

func validateControlLifetimeUnlaunched(l *ControlLifetime, writing bool) error {
	if err := l.validateLifetimeOperation(writing); err != nil {
		return err
	}
	if l.ownerKind != task.LifetimeOwnerWorkspace && l.ownerKind != task.LifetimeOwnerInspection {
		return fmt.Errorf("%w: unlaunched fences require an auxiliary owner", task.ErrInvalidEnum)
	}
	return nil
}

func sealControlLifetimeUnlaunchedTx(tx *ControlTransaction, l *ControlLifetime, now func() time.Time) (*task.LifetimeUnlaunchedRecord, error) {
	existing, readErr := readControlLifetimeUnlaunchedTx(tx, l)
	if readErr == nil {
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	if err := ensureNoNativeLifetimeRecordsTx(tx, l); err != nil {
		return nil, err
	}
	observedAt := now()
	if observedAt.IsZero() || observedAt.Before(l.deadline) {
		return nil, task.ErrLifetimeDeadlineNotExpired
	}
	candidate := task.LifetimeUnlaunchedRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        l.rootID,
		TaskID:        l.operationID,
		OwnerKind:     l.ownerKind,
		OwnerSHA256:   l.ownerSHA256,
		Deadline:      l.deadlineText(),
		Cause:         task.LifetimeUnlaunchedCauseDeadlineExpired,
		ObservedAt:    observedAt.UTC().Format(time.RFC3339Nano),
	}
	if err := task.ValidateLifetimeUnlaunchedRecord(&candidate); err != nil {
		return nil, err
	}
	created, putErr := tx.Put(controlLifetimeUnlaunchedName, candidate)
	if created && putErr == nil {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeUnlaunchedTx(tx, l)
	if winnerErr != nil {
		return nil, errors.Join(putErr, winnerErr)
	}
	candidate.ObservedAt = winner.ObservedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, errors.Join(putErr, task.ErrIdentityMismatch)
	}
	return winner, putErr
}

func readControlLifetimeUnlaunchedTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeUnlaunchedRecord, error) {
	var record task.LifetimeUnlaunchedRecord
	if err := tx.Read(controlLifetimeUnlaunchedName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeUnlaunchedName, err)
	}
	if err := task.ValidateLifetimeUnlaunchedRecord(&record); err != nil {
		return nil, err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.OwnerKind != l.ownerKind || record.OwnerSHA256 != l.ownerSHA256 || record.Deadline != l.deadlineText() {
		return nil, task.ErrIdentityMismatch
	}
	if err := tx.Verify(controlLifetimeUnlaunchedName, record); err != nil {
		return nil, err
	}
	if err := ensureNoNativeLifetimeRecordsTx(tx, l); err != nil {
		return nil, fmt.Errorf("%w: unlaunched fence conflicts with native lifetime state: %w", task.ErrEvidenceFault, err)
	}
	return &record, nil
}

func rejectLifetimeLaunchPermissionTx(tx *ControlTransaction, l *ControlLifetime) error {
	fence, err := readControlLifetimeUnlaunchedTx(tx, l)
	if err == nil {
		if fence == nil {
			return fmt.Errorf("%w: unlaunched reader returned nil record", task.ErrEvidenceFault)
		}
		return task.ErrLifetimeUnlaunched
	}
	if !task.RecordAbsent(err) {
		return err
	}
	completion, completionErr := readControlLifetimeWorkerWaitTx(tx, l)
	if completionErr == nil && task.LifetimeWorkerWaitProvesAbsence(completion) {
		return task.ErrTerminalTask
	}
	if completionErr != nil && !task.RecordAbsent(completionErr) {
		return completionErr
	}
	termination, terminationErr := readControlLifetimeTerminationPresenceTx(tx, l)
	if terminationErr == nil {
		if termination == nil || !termination.ScopeEmpty {
			return fmt.Errorf("%w: termination reader returned invalid record", task.ErrEvidenceFault)
		}
		return task.ErrTerminalTask
	}
	if !task.RecordAbsent(terminationErr) {
		return terminationErr
	}
	return nil
}

func ensureNoNativeLifetimeRecordsTx(tx *ControlTransaction, l *ControlLifetime) error {
	if _, err := readControlLifetimeIntentTx(tx, l); err == nil {
		return fmt.Errorf("%w: lifetime intent already exists", task.ErrAlreadyStarted)
	} else if !task.RecordAbsent(err) {
		return err
	}
	if _, _, err := readControlLifetimeRegistrationTx(tx, l); err == nil {
		return fmt.Errorf("%w: lifetime registration already exists", task.ErrAlreadyStarted)
	} else if !task.RecordAbsent(err) {
		return err
	}
	if _, err := readControlLifetimeWorkerTx(tx, l); err == nil {
		return fmt.Errorf("%w: lifetime worker already exists", task.ErrAlreadyStarted)
	} else if !task.RecordAbsent(err) {
		return err
	}
	if _, err := readControlLifetimeWorkerWaitTx(tx, l); err == nil {
		return fmt.Errorf("%w: lifetime worker Wait already exists", task.ErrAlreadyStarted)
	} else if !task.RecordAbsent(err) {
		return err
	}
	if _, err := readControlLifetimeTerminationPresenceTx(tx, l); err == nil {
		return task.ErrTerminalTask
	} else if !task.RecordAbsent(err) {
		return err
	}
	return rejectOrphanedLifetimeLaunchesTx(tx, l)
}

func rejectOrphanedLifetimeLaunchesTx(tx *ControlTransaction, l *ControlLifetime) error {
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches; sequence++ {
		var record task.LifetimeLaunchRecord
		name := controlLifetimeLaunchName(sequence)
		err := tx.Read(name, &record)
		if task.RecordAbsent(err) {
			continue
		}
		if err != nil {
			return controlLifetimeReadError(name, err)
		}
		if err := task.ValidateLifetimeLaunchRecord(&record); err != nil {
			return err
		}
		if record.RootID != l.rootID || record.TaskID != l.operationID {
			return task.ErrIdentityMismatch
		}
		if err := tx.Verify(name, record); err != nil {
			return err
		}
		return fmt.Errorf("%w: lifetime launch record already exists", task.ErrAlreadyStarted)
	}
	return nil
}

func readControlLifetimeTerminationPresenceTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeTerminationRecord, error) {
	var record task.LifetimeTerminationRecord
	if err := tx.Read(controlLifetimeTerminationName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeTerminationName, err)
	}
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return nil, err
	}
	registration, registrationDigest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	launches, err := readControlLifetimeLaunchesTx(tx, l, registrationDigest)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err := validateControlLifetimeTermination(l, record, registrationDigest, launches); err != nil {
		return nil, err
	}
	if err := tx.Verify(controlLifetimeTerminationName, record); err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, task.ErrEvidenceFault
	}
	return &record, nil
}
