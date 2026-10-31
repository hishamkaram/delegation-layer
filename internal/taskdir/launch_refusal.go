package taskdir

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TaskLaunchRefusedContext is the canonical ordinary-task refusal reader.
// Terminal publication and permanent stops remain their own authorities;
// this reader never creates a replacement outcome, deadline, or native fact.
func (td *TaskDir) TaskLaunchRefusedContext(ctx context.Context) (bool, error) {
	_, digest, err := td.permanentLaunchRefusalContext(ctx)
	return digest != "" && err == nil, err
}

// Outcome wins first; otherwise ReadStopRecords supplies requests in stable ID
// order. Hash the canonical validated record, never a new composite proof.
func (td *TaskDir) permanentLaunchRefusalContext(ctx context.Context) (string, string, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return "", "", err
	}
	_, meta, specHash, metaHash, err := td.loadAndValidatePreparedSetContext(ctx)
	if err != nil {
		return "", "", err
	}
	outcome, err := td.readWinnerRecord(meta, specHash, metaHash)
	if contextErr := ctx.Err(); contextErr != nil {
		return "", "", contextErr
	}
	if err == nil {
		data, marshalErr := marshalControlRecord(outcome)
		if marshalErr != nil {
			return "", "", marshalErr
		}
		return "outcome.json", task.ComputeSHA256(data), nil
	}
	if !errors.Is(err, errNoOutcome) {
		return "", "", err
	}
	records, err := td.ReadStopRecords()
	if err = errors.Join(err, ctx.Err()); err != nil {
		return "", "", err
	}
	for _, record := range records {
		if record.Request != nil && task.PermanentLaunchRefusalCause(record.Request.Cause) {
			data, marshalErr := marshalControlRecord(record.Request)
			if marshalErr != nil {
				return "", "", marshalErr
			}
			return filepath.Join("stop", record.Request.RequestID+stopRequestSuffix), task.ComputeSHA256(data), nil
		}
	}
	return "", "", nil
}

func (td *TaskDir) requireTaskLaunchPermissionContext(ctx context.Context) error {
	refused, err := td.TaskLaunchRefusedContext(ctx)
	if err != nil {
		return err
	}
	if refused {
		return task.ErrTerminalTask
	}
	return nil
}

// UserStopRequested is the synchronous guardian callback's durable user-stop
// view. Automatic abandonment remains a separate permanent refusal cause.
func (td *TaskDir) UserStopRequested() (bool, error) {
	return td.UserStopRequestedContext(context.Background())
}

// UserStopRequestedContext includes observed stops. An acknowledgement never
// revokes the original cancellation or authorizes a later provider launch.
func (td *TaskDir) UserStopRequestedContext(ctx context.Context) (bool, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, err
	}
	records, err := td.ReadStopRecords()
	if err = errors.Join(err, ctx.Err()); err != nil {
		return false, err
	}
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "user" {
			return true, nil
		}
	}
	return false, nil
}

// ProveUnlaunchedTaskContext elects the existing lifetime owner before taking
// the run lock. A busy owner is not proof. Durable refusal fences later owners.
func (td *TaskDir) ProveUnlaunchedTaskContext(ctx context.Context) (proven bool, resultErr error) {
	proven, _, resultErr = td.ProveUnlaunchedTaskReferenceContext(ctx)
	return proven, resultErr
}

// ProveUnlaunchedTaskReferenceContext returns the validated permanent refusal's
// canonical digest under the same lifetime/run ownership as the existing proof.
// Unlike fencing, this also accepts submitted tasks and never writes a refusal.
func (td *TaskDir) ProveUnlaunchedTaskReferenceContext(ctx context.Context) (proven bool, referenceSHA256 string, resultErr error) {
	defer func() {
		if resultErr != nil {
			proven, referenceSHA256 = false, ""
		}
	}()
	if td == nil {
		return false, "", task.ErrInvalidPermit
	}
	owner, err := td.TryAcquireLifetimeOwnerContext(ctx)
	if unlaunchedTaskLockBusy(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	return td.withUnlaunchedTaskRunLockContext(ctx, false)
}

// ProveUnlaunchedTaskOwnedContext requires the caller's lifetime-owner lease.
// It shares the conditional fence's run critical section and evidence checks.
func (td *TaskDir) ProveUnlaunchedTaskOwnedContext(ctx context.Context) (proven bool, resultErr error) {
	proven, _, resultErr = td.withUnlaunchedTaskRunLockContext(ctx, false)
	return proven, resultErr
}

// FenceUnlaunchedTaskContext closes admission only after the caller has excluded
// all earlier Dispatch/control invocations. Storage owns every remaining fact:
// admission, lifetime and run leases span eligibility, refusal write and proof.
// No submission, native lifetime, provider start or terminal record is created.
func (td *TaskDir) FenceUnlaunchedTaskContext(ctx context.Context) (proven bool, referenceSHA256 string, resultErr error) {
	defer func() {
		if resultErr != nil {
			proven, referenceSHA256 = false, ""
		}
	}()
	if td == nil {
		return false, "", task.ErrInvalidPermit
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return false, "", err
	}
	if err := td.store.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return false, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	if err := td.admissionLock.LockEXNonblockingContext(ctx); err != nil {
		if unlaunchedTaskLockBusy(err) {
			return false, "", nil
		}
		return false, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, td.admissionLock.Unlock()) }()
	owner, err := td.TryAcquireLifetimeOwnerContext(ctx)
	if unlaunchedTaskLockBusy(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	return td.withUnlaunchedTaskRunLockContext(ctx, true)
}

// The caller holds lifetime ownership; fencing additionally holds admission.
// Both paths use this run critical section without reacquiring the run lock.
func (td *TaskDir) withUnlaunchedTaskRunLockContext(ctx context.Context, fence bool) (proven bool, referenceSHA256 string, resultErr error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, "", err
	}
	if err := td.runLock.LockEXNonblockingContext(ctx); unlaunchedTaskLockBusy(err) {
		return false, "", nil
	} else if err != nil {
		return false, "", err
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock()) }()
	if _, _, _, _, err := td.loadAndValidatePreparedSetContext(ctx); err != nil {
		return false, "", err
	}
	eligible, err := td.unlaunchedTaskEvidenceContext(ctx)
	if err != nil || !eligible {
		return false, "", err
	}
	if fence {
		// Inspect both primary paths: a receipt's validating reader depends
		// on submit.json and cannot establish an orphaned receipt's absence.
		unsubmitted, readErr := td.taskRecordsAbsentContext(ctx, "submit.json", "supervisor.ref.json")
		if readErr != nil || !unsubmitted {
			return false, "", readErr
		}
	}
	name, digest, err := td.permanentLaunchRefusalContext(ctx)
	if err != nil {
		return false, "", err
	}
	if name != "outcome.json" {
		unselected, readErr := td.taskRecordsAbsentContext(ctx, "result.txt", "publish.reject")
		if readErr != nil || !unselected {
			return false, "", readErr
		}
	}
	return td.provePermanentRefusalLockedContext(ctx, fence, name, digest)
}

func unlaunchedTaskLockBusy(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) })
}

func (td *TaskDir) taskRecordsAbsentContext(ctx context.Context, names ...string) (bool, error) {
	for _, name := range names {
		present, err := td.store.entryExists(filepath.Join(td.Dir, name))
		if err = errors.Join(err, ctx.Err()); err != nil || present {
			return false, err
		}
	}
	return true, nil
}

func (td *TaskDir) provePermanentRefusalLockedContext(ctx context.Context, fence bool, name, digest string) (bool, string, error) {
	var err error
	if digest == "" && fence {
		if err = td.recordAutomaticAbandonmentContext(ctx); err != nil {
			return false, "", err
		}
		name, digest, err = td.permanentLaunchRefusalContext(ctx)
	}
	if err != nil || digest == "" {
		return false, "", err
	}
	if fence {
		if err = td.acknowledgeRecord(name); err != nil {
			return false, "", err
		}
	}
	if err = ctx.Err(); err != nil {
		return false, "", err
	}
	return true, digest, nil
}

func (td *TaskDir) unlaunchedTaskEvidenceContext(ctx context.Context) (bool, error) {
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return false, err
	}
	if evidence.Intent != nil || evidence.Registration != nil || evidence.Worker != nil || evidence.WorkerWait != nil || evidence.Termination != nil || len(evidence.Launches) != 0 {
		return false, nil
	}
	absent, err := td.taskRecordsAbsentContext(ctx, "provider.start", "provider.started.json", "provider.exit", "provider.ref.json")
	if err != nil || !absent {
		return false, err
	}
	return td.unlaunchedTaskRawEmptyContext(ctx)
}

// Empty pre-start capture files can remain after initialization fails. Any
// captured bytes or unreadable raw evidence make past execution uncertain.
func (td *TaskDir) unlaunchedTaskRawEmptyContext(ctx context.Context) (bool, error) {
	names, err := td.admittedRawNames()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		_, readErr := td.store.readBytesContext(ctx, filepath.Join(td.Dir, "raw", name), 0)
		if readErr != nil && !task.RecordAbsent(readErr) {
			return false, fmt.Errorf("%w: pre-start raw evidence %s: %w", task.ErrEvidenceFault, name, readErr)
		}
	}
	return true, nil
}

func (td *TaskDir) recordAutomaticAbandonmentContext(ctx context.Context) error {
	id := task.ComputeSHA256([]byte(task.StopCauseAutomaticAbandonment + ":" + td.TaskID))[:32]
	candidate, err := td.newStopRequest(id, task.StopCauseAutomaticAbandonment, time.Time{})
	if err != nil {
		return err
	}
	if err = td.store.mkdirContext(ctx, filepath.Join(td.Dir, "stop"), td.store.faultInjector); err != nil {
		return err
	}
	name := filepath.Join("stop", id+stopRequestSuffix)
	readSame := func() (bool, error) {
		winner, readErr := td.ReadStopRequest(id)
		return readErr == nil && reflect.DeepEqual(candidate, winner), readErr
	}
	writeErr := td.recordSameContext(ctx, name, candidate, readSame)
	same, readErr := readSame()
	if task.RecordAbsent(readErr) && writeErr != nil {
		// One bounded replay uses the exact original candidate. A second failure
		// stays unresolved and never authorizes a replacement.
		writeErr = errors.Join(writeErr, td.recordSameContext(ctx, name, candidate, readSame))
		same, readErr = readSame()
	}
	if readErr != nil {
		return errors.Join(writeErr, readErr)
	}
	if !same {
		return errors.Join(writeErr, task.ErrIdentityMismatch)
	}
	return errors.Join(writeErr, td.acknowledgeRecord(name), ctx.Err())
}
