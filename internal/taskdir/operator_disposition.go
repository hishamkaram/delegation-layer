package taskdir

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const operatorDispositionName = "operator-disposition.json"

// OperatorDispositionNative supplies positive native observations while this
// storage owner retains all task exclusions. Unknown facts must fail closed.
type OperatorDispositionNative interface {
	CurrentBootID() (string, error)
	SameBoot(recorded, current string) bool
	ProcessAbsent(pid int, start string) (bool, error)
}

// PrepareOperatorDispositionContext observes an exact ended task under all
// existing execution exclusions. Its caller retains the current supervisor
// generation lease and supplies a canonically matched queue observation.
// It never creates a lock, probes write support or persists preparation state.
func (td *TaskDir) PrepareOperatorDispositionContext(ctx context.Context, queue task.OperatorDispositionQueue, native OperatorDispositionNative) (subject task.OperatorDispositionSubject, challenge string, resultErr error) {
	if err := td.operatorDispositionContext(ctx); err != nil {
		return subject, "", err
	}
	resultErr = td.withTaskObservationExclusionContext(ctx, false, false, func() error {
		var err error
		subject, err = td.operatorDispositionSubjectContext(ctx)
		if err != nil {
			return err
		}
		if err = td.requireOperatorDispositionEligibleContext(ctx, subject, queue, native); err != nil {
			return err
		}
		challenge, err = task.OperatorDispositionChallenge(subject)
		return err
	})
	return subject, challenge, resultErr
}

// ConfirmOperatorDispositionContext is the sole sidecar writer. A matching
// immutable winner is returned before clock construction or another write.
// No terminal, native-lifetime or resource record is changed.
func (td *TaskDir) ConfirmOperatorDispositionContext(ctx context.Context, queue task.OperatorDispositionQueue, challenge string, native OperatorDispositionNative) (winner *task.OperatorDispositionRecord, resultErr error) {
	if err := td.operatorDispositionContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateSHA256(challenge); err != nil {
		return nil, err
	}
	resultErr = td.withTaskObservationExclusionContext(ctx, false, false, func() error {
		var err error
		winner, err = td.confirmOperatorDispositionWithExclusionContext(ctx, queue, challenge, native)
		return err
	})
	return winner, resultErr
}

// ReadOperatorDispositionContext validates immutable identity only. This
// read grants neither native closure nor permission for maintenance; that
// consumer must recheck owners, the exact ended queue row and native absence.
func (td *TaskDir) ReadOperatorDispositionContext(ctx context.Context) (*task.OperatorDispositionRecord, error) {
	if err := td.operatorDispositionContext(ctx); err != nil {
		return nil, err
	}
	pin, err := td.store.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	return td.readOperatorDispositionWithPinContext(ctx, pin)
}

func (td *TaskDir) readOperatorDispositionWithPinContext(ctx context.Context, pin RootPin) (*task.OperatorDispositionRecord, error) {
	var record task.OperatorDispositionRecord
	if err := td.store.readRecordContext(ctx, filepath.Join(td.Dir, operatorDispositionName), &record); err != nil {
		return nil, err
	}
	if err := task.ValidateOperatorDispositionRecord(&record); err != nil {
		return nil, errors.Join(task.ErrEvidenceFault, err)
	}
	subject, err := td.operatorDispositionSubjectWithPinContext(ctx, pin)
	if err != nil {
		return nil, errors.Join(task.ErrEvidenceFault, err)
	}
	if record.Subject != subject {
		return nil, fmt.Errorf("%w: operator disposition no longer matches immutable task evidence", task.ErrIdentityMismatch)
	}
	return &record, nil
}

// ReadSupervisorMaintenancePermissionContext is maintenance-specific. Its
// caller validates the current queue mapping under its generation lease; this
// owner revalidates the sidecar and task exclusions through the observation.
func (td *TaskDir) ReadSupervisorMaintenancePermissionContext(ctx context.Context, queue task.OperatorDispositionQueue, native OperatorDispositionNative) (allowed bool, resultErr error) {
	if err := td.operatorDispositionContext(ctx); err != nil {
		return false, err
	}
	// A task without a disposition retains its original policy and error paths.
	_, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, operatorDispositionName), task.MaxControlRecordSize)
	if task.RecordAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resultErr = td.withTaskObservationExclusionContext(ctx, false, false, func() error {
		record, readErr := td.ReadOperatorDispositionContext(ctx)
		if readErr != nil {
			return readErr
		}
		if readErr = td.requireOperatorDispositionNativeContext(ctx, record.Subject, queue, native); readErr != nil {
			return readErr
		}
		allowed = true
		return nil
	})
	return allowed, resultErr
}

func (td *TaskDir) operatorDispositionContext(ctx context.Context) error {
	if ctx == nil || td == nil || td.store == nil {
		return task.ErrInvalidPermit
	}
	return ctx.Err()
}

func (td *TaskDir) operatorDispositionSubjectContext(ctx context.Context) (task.OperatorDispositionSubject, error) {
	pin, err := td.store.PinRootContext(ctx)
	if err != nil {
		return task.OperatorDispositionSubject{}, err
	}
	return td.operatorDispositionSubjectWithPinContext(ctx, pin)
}

func (td *TaskDir) operatorDispositionSubjectWithPinContext(ctx context.Context, pin RootPin) (task.OperatorDispositionSubject, error) {
	var subject task.OperatorDispositionSubject
	var err error
	if _, _, _, _, err = td.loadAndValidatePreparedSetContext(ctx); err != nil {
		return subject, err
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return subject, err
	}
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return subject, err
	}
	if evidence.Intent == nil || evidence.Registration == nil || evidence.Worker == nil {
		return subject, fmt.Errorf("%w: settlement requires complete intent, registration and worker identity", task.ErrEvidenceFault)
	}
	subject = task.OperatorDispositionSubject{RootDevice: pin.Device(), RootInode: pin.Inode(), Receipt: *receipt, BootID: evidence.Registration.BootID, Permission: task.OperatorPermissionSupervisorMaintenance}
	for _, item := range []struct {
		record any
		digest *string
	}{{evidence.Intent, &subject.IntentSHA256}, {evidence.Registration, &subject.RegistrationSHA256}, {evidence.Worker, &subject.WorkerSHA256}} {
		data, marshalErr := task.MarshalCanonical(item.record)
		if marshalErr != nil {
			return subject, marshalErr
		}
		*item.digest = task.ComputeSHA256(data)
	}
	subject.LaunchSetSHA256, err = task.LifetimeLaunchSetDigest(evidence.Launches)
	return subject, errors.Join(err, ctx.Err())
}

func (td *TaskDir) requireOperatorDispositionEligibleContext(ctx context.Context, subject task.OperatorDispositionSubject, queue task.OperatorDispositionQueue, native OperatorDispositionNative) error {
	inspection, err := td.InspectContext(ctx)
	if err != nil {
		return err
	}
	if inspection.Publication != task.PublicationUnknown || inspection.SealExists || inspection.OutcomeExists {
		return fmt.Errorf("%w: real publication evidence must be collected", task.ErrTerminalTask)
	}
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return err
	}
	if closure.Closed() {
		return fmt.Errorf("%w: canonical lifetime closure already exists", task.ErrTerminalTask)
	}
	return td.requireOperatorDispositionNativeContext(ctx, subject, queue, native)
}

func (td *TaskDir) requireOperatorDispositionNativeContext(ctx context.Context, subject task.OperatorDispositionSubject, queue task.OperatorDispositionQueue, native OperatorDispositionNative) error {
	if native == nil {
		return task.ErrInvalidPermit
	}
	if !queue.Ended || queue.NumericTaskID != subject.Receipt.NumericTaskID || queue.Label != subject.Receipt.Label {
		return fmt.Errorf("%w: settlement requires the exact matching ended supervisor row", task.ErrIdentityMismatch)
	}
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return err
	}
	if err = operatorDispositionCooperativeEvidence(evidence); err != nil {
		return err
	}
	registration, worker := evidence.Registration, evidence.Worker
	if err = ctx.Err(); err != nil {
		return err
	}
	boot, err := native.CurrentBootID()
	if err != nil {
		return err
	}
	if !native.SameBoot(registration.BootID, boot) || !native.SameBoot(worker.BootID, boot) {
		return fmt.Errorf("%w: settlement requires verified current-boot identities; use explicit recovery for a previous boot", task.ErrIdentityMismatch)
	}
	if err = requireOperatorProcessAbsent(ctx, native, worker.PID, worker.ProcessStart); err != nil {
		return err
	}
	for _, launch := range evidence.Launches {
		if err = requireOperatorProcessAbsent(ctx, native, launch.PID, launch.ProcessStart); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func requireOperatorProcessAbsent(ctx context.Context, native OperatorDispositionNative, pid int, start string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	absent, err := native.ProcessAbsent(pid, start)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("%w: an exact recorded worker or launch is still live", task.ErrLockBusy)
	}
	return ctx.Err()
}

func (td *TaskDir) commitOperatorDispositionContext(ctx context.Context, candidate *task.OperatorDispositionRecord) (*task.OperatorDispositionRecord, error) {
	readSame := func() (bool, error) {
		old, err := td.ReadOperatorDispositionContext(ctx)
		return err == nil && *old == *candidate, err
	}
	writeErr := td.recordSameContext(ctx, operatorDispositionName, candidate, readSame)
	for attempt := 0; ; attempt++ {
		winner, readErr := td.ReadOperatorDispositionContext(ctx)
		if readErr == nil {
			if *winner != *candidate {
				return winner, errors.Join(writeErr, task.ErrInvariantFault)
			}
			return winner, writeErr
		}
		if !task.RecordAbsent(readErr) || attempt != 0 || ctx.Err() != nil {
			return nil, errors.Join(writeErr, readErr)
		}
		// Only this authoritative absence permits replay of the SAME candidate.
		writeErr = errors.Join(writeErr, td.recordSameContext(ctx, operatorDispositionName, candidate, readSame))
	}
}

func (td *TaskDir) confirmOperatorDispositionWithExclusionContext(ctx context.Context, queue task.OperatorDispositionQueue, challenge string, native OperatorDispositionNative) (*task.OperatorDispositionRecord, error) {
	existing, err := td.ReadOperatorDispositionContext(ctx)
	if err == nil {
		if existing.Challenge != challenge {
			return nil, fmt.Errorf("%w: another operator disposition already won", task.ErrInvariantFault)
		}
		return existing, nil
	}
	if !task.RecordAbsent(err) {
		return nil, err
	}
	subject, err := td.operatorDispositionSubjectContext(ctx)
	if err != nil {
		return nil, err
	}
	actual, err := task.OperatorDispositionChallenge(subject)
	if err != nil {
		return nil, err
	}
	if actual != challenge {
		return nil, fmt.Errorf("%w: settlement challenge is stale; prepare and review the exact task again", task.ErrIdentityMismatch)
	}
	if err = td.requireOperatorDispositionEligibleContext(ctx, subject, queue, native); err != nil {
		return nil, err
	}
	// The existing exclusions already retain maintenance SH. Use the same
	// support validation as the normal opener without reacquiring its lock.
	if err = td.store.probeFilesystemSupport(ctx); err != nil {
		return nil, err
	}
	after, err := td.operatorDispositionSubjectContext(ctx)
	if err != nil {
		return nil, err
	}
	if after != subject {
		return nil, fmt.Errorf("%w: settlement subject changed before commit", task.ErrIdentityMismatch)
	}
	candidate := &task.OperatorDispositionRecord{SchemaVersion: task.SchemaVersion, Subject: subject, Challenge: challenge, ConfirmedAt: timestamp()}
	return td.commitOperatorDispositionContext(ctx, candidate)
}

func operatorDispositionCooperativeEvidence(evidence task.LifetimeEvidence) error {
	registration, worker := evidence.Registration, evidence.Worker
	if registration == nil || worker == nil || registration.Assurance != task.LifetimeAssuranceCooperative || registration.ScopeKind != task.LifetimeScopeProcessGroup || registration.OwnerKind != task.LifetimeOwnerTask {
		return fmt.Errorf("%w: settlement requires an unresolved cooperative task lifetime", task.ErrEvidenceFault)
	}
	return nil
}
