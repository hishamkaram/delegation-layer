package taskdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	lifetimeDirectory        = "lifetime"
	lifetimeIntentName       = "intent.json"
	lifetimeRegistrationName = "registration.json"
	lifetimeWorkerName       = "worker.json"
	lifetimeWorkerWaitName   = "worker-wait.json"
	lifetimeTerminationName  = "terminated.json"
	lifetimeOwnerLockName    = "owner.lock"
	lifetimeLaunchSuffix     = ".launch.json"
)

type (
	LifetimeIntentFacts struct {
		ScopeKind string
		Assurance string
		ScopePlan string
		BootID    string
		Deadline  time.Time
	}

	LifetimeRegistrationFacts struct {
		ScopeKind string
		Assurance string
		ScopeID   string
		BootID    string
		Deadline  time.Time
	}

	LifetimeLaunchFacts struct {
		Sequence     uint64
		PID          int
		ProcessStart string
		PGID         int
	}

	LifetimeWorkerFacts struct {
		PID          int
		ProcessStart string
		BootID       string
	}

	LifetimeWorkerWaitFacts struct {
		WorkerSHA256 string
		ChildPID     int
		Result       task.LifetimeWorkerWaitResult
		ExitCode     *int
		ObservedAt   time.Time
	}

	LifetimeOwnerLease struct {
		mu                      sync.Mutex
		maintenance             *LockFile
		owner                   *LockFile
		ownerClosed             bool
		maintenanceUnlockFailed bool
		maintenanceUnlocked     bool
		released                bool
		releaseErr              error
	}
)

// AcquireLifetimeOwner serializes the only process-scope transition owner for
// a task. It also retains the root maintenance lease so cleanup cannot remove
// the durable scope while the guardian is active.
func (td *TaskDir) AcquireLifetimeOwner(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if td == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := td.store.maintLock.LockSH(ctx); err != nil {
		return nil, err
	}
	releaseMaintenance := true
	defer func() {
		if releaseMaintenance {
			resultErr = errors.Join(resultErr, td.store.maintLock.Unlock())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(td.Dir, lifetimeDirectory)
	if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
		return nil, err
	}
	owner, err := td.store.openLock(filepath.Join(dir, lifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return nil, err
	}
	if err = owner.LockEX(ctx); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	releaseMaintenance = false
	return &LifetimeOwnerLease{maintenance: td.store.maintLock, owner: owner}, nil
}

// TryAcquireLifetimeOwner is the nonblocking recovery probe. A busy result
// means the current guardian still owns the transition.
func (td *TaskDir) TryAcquireLifetimeOwner() (_ *LifetimeOwnerLease, resultErr error) {
	return td.tryAcquireLifetimeOwner(context.Background())
}

// TryAcquireLifetimeOwnerContext is the context-aware nonblocking recovery
// probe. Cancellation is checked before any lock or filesystem state changes.
func (td *TaskDir) TryAcquireLifetimeOwnerContext(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return td.tryAcquireLifetimeOwner(ctx)
}

func (td *TaskDir) tryAcquireLifetimeOwner(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if td == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := td.store.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return nil, err
	}
	releaseMaintenance := true
	defer func() {
		if releaseMaintenance {
			resultErr = errors.Join(resultErr, td.store.maintLock.Unlock())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(td.Dir, lifetimeDirectory)
	if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
		return nil, err
	}
	owner, err := td.store.openLock(filepath.Join(dir, lifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return nil, err
	}
	if err = owner.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	releaseMaintenance = false
	return &LifetimeOwnerLease{maintenance: td.store.maintLock, owner: owner}, nil
}

func (l *LifetimeOwnerLease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return l.releaseErr
	}
	var ownerErr error
	if !l.ownerClosed {
		ownerErr = l.owner.Close()
		if ownerErr == nil {
			l.ownerClosed = true
		}
	}

	var maintenanceErr error
	if !l.maintenanceUnlocked {
		if l.maintenanceUnlockFailed {
			// LockFile.Unlock decrements its local shared count before the
			// underlying flock operation. Restore that reference before retrying
			// an unlock that failed, so the kernel lease remains authoritative.
			maintenanceErr = l.maintenance.LockSHNonblocking()
		}
		if maintenanceErr == nil {
			maintenanceErr = l.maintenance.Unlock()
		}
		if maintenanceErr == nil {
			l.maintenanceUnlocked = true
			l.maintenanceUnlockFailed = false
		} else {
			l.maintenanceUnlockFailed = true
		}
	}

	l.releaseErr = errors.Join(ownerErr, maintenanceErr)
	l.released = l.ownerClosed && l.maintenanceUnlocked && l.releaseErr == nil
	return l.releaseErr
}

// DuplicateControlLeaseFiles gives the one-shot supervisor control owner
// inherited copies of the maintenance and lifetime-owner locks. The caller
// retains normal release ownership and waits for the control owner to finish;
// the owner-held copies protect the same transition if the caller exits first.
func (l *LifetimeOwnerLease) DuplicateControlLeaseFiles() ([]*os.File, error) {
	if l == nil {
		return nil, task.ErrInvalidPermit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.ownerClosed || l.maintenanceUnlocked {
		return nil, task.ErrPermitAlreadyUsed
	}
	maintenance, err := l.maintenance.DuplicateHeldFile("delegate-task-maintenance-lease")
	if err != nil {
		return nil, err
	}
	owner, err := l.owner.DuplicateHeldFile("delegate-task-lifetime-owner")
	if err != nil {
		return nil, errors.Join(err, maintenance.Close())
	}
	return []*os.File{maintenance, owner}, nil
}

func (td *TaskDir) RecordLifetimeIntent(facts LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	_, _, _, spec, meta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	ownerDigest, err := task.LifetimeTaskOwnerSHA256(spec, meta)
	if err != nil {
		return nil, err
	}
	record := task.LifetimeIntentRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        td.store.RootID,
		TaskID:        td.TaskID,
		OwnerKind:     task.LifetimeOwnerTask,
		OwnerSHA256:   ownerDigest,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopePlan:     facts.ScopePlan,
		BootID:        facts.BootID,
		Deadline:      facts.Deadline.UTC().Format(time.RFC3339Nano),
		CreatedAt:     timestamp(),
	}
	if err = task.ValidateLifetimeIntentRecord(&record); err != nil {
		return nil, err
	}
	if err = td.store.mkdir(filepath.Join(td.Dir, lifetimeDirectory), td.store.faultInjector); err != nil {
		return nil, err
	}
	err = td.recordSame(filepath.Join(lifetimeDirectory, lifetimeIntentName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeIntent()
		if readErr != nil {
			return false, readErr
		}
		record.CreatedAt = existing.CreatedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeIntent()
}

func (td *TaskDir) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	var record task.LifetimeIntentRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeIntentName)
	if err := td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeIntentRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeOwner(record.RootID, record.TaskID, record.OwnerKind, record.OwnerSHA256); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) RecordLifetimeRegistration(facts LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	_, _, _, spec, meta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	ownerDigest, err := task.LifetimeTaskOwnerSHA256(spec, meta)
	if err != nil {
		return nil, err
	}
	record := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        td.store.RootID,
		TaskID:        td.TaskID,
		OwnerKind:     task.LifetimeOwnerTask,
		OwnerSHA256:   ownerDigest,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopeID:       facts.ScopeID,
		BootID:        facts.BootID,
		Deadline:      facts.Deadline.UTC().Format(time.RFC3339Nano),
		CreatedAt:     timestamp(),
	}
	if err = task.ValidateLifetimeRegistrationRecord(&record); err != nil {
		return nil, err
	}
	if existing, readErr := td.ReadLifetimeRegistration(); readErr == nil {
		record.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(existing, &record) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, td.acknowledgeRecord(filepath.Join(lifetimeDirectory, lifetimeRegistrationName))
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	intent, err := td.ReadLifetimeIntent()
	if err != nil {
		return nil, err
	}
	if !task.LifetimeIntentMatchesRegistration(intent, &record) {
		return nil, task.ErrIdentityMismatch
	}
	err = td.recordSame(filepath.Join(lifetimeDirectory, lifetimeRegistrationName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeRegistration()
		if readErr != nil {
			return false, readErr
		}
		record.CreatedAt = existing.CreatedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeRegistration()
}

func (td *TaskDir) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	var record task.LifetimeRegistrationRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
	if err := td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeRegistrationRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeOwner(record.RootID, record.TaskID, record.OwnerKind, record.OwnerSHA256); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) LifetimeRegistrationDigest() (string, error) {
	if _, err := td.ReadLifetimeRegistration(); err != nil {
		return "", err
	}
	data, err := td.store.readBytes(filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName), task.MaxControlRecordSize)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

// LifetimeWorkerDigest returns the digest of the canonical persisted worker
// winner using taskdir's persisted-byte identity semantics.
func (td *TaskDir) LifetimeWorkerDigest() (string, error) {
	if _, err := td.ReadLifetimeWorker(); err != nil {
		return "", err
	}
	data, err := td.store.readBytes(filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName), task.MaxControlRecordSize)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func (td *TaskDir) RecordLifetimeWorker(facts LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	registration, err := td.ReadLifetimeRegistration()
	if err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		return nil, err
	}
	record := task.LifetimeWorkerRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		PID:                facts.PID,
		ProcessStart:       facts.ProcessStart,
		BootID:             facts.BootID,
		StartedAt:          timestamp(),
	}
	if err = task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	if record.BootID != registration.BootID {
		return nil, task.ErrIdentityMismatch
	}
	err = td.recordSame(filepath.Join(lifetimeDirectory, lifetimeWorkerName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeWorker()
		if readErr != nil {
			return false, readErr
		}
		record.StartedAt = existing.StartedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeWorker()
}

func (td *TaskDir) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	registration, err := td.ReadLifetimeRegistration()
	if err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
	if err = td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err = task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	if record.RootID != td.store.RootID || record.TaskID != td.TaskID || record.RegistrationSHA256 != registrationDigest || record.BootID != registration.BootID {
		return nil, task.ErrIdentityMismatch
	}
	return &record, nil
}

// RecordLifetimeWorkerWait persists the create-once managed-worker completion
// disposition. An empty WorkerSHA256 is accepted when no worker started or
// when an exact scope has no persisted worker identity to bind to.
func (td *TaskDir) RecordLifetimeWorkerWait(facts LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	registration, registrationDigest, worker, workerDigest, err := td.lifetimeWorkerWaitBinding()
	if err != nil {
		return nil, err
	}
	observedAt := facts.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	record := task.LifetimeWorkerWaitRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		WorkerSHA256:       facts.WorkerSHA256,
		ChildPID:           facts.ChildPID,
		Result:             facts.Result,
		ExitCode:           facts.ExitCode,
		ObservedAt:         observedAt.UTC().Format(time.RFC3339Nano),
	}
	if err = validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	err = td.recordSame(filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeWorkerWait()
		if readErr != nil {
			return false, readErr
		}
		record.ObservedAt = existing.ObservedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeWorkerWait()
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (td *TaskDir) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	registration, registrationDigest, worker, workerDigest, err := td.lifetimeWorkerWaitBinding()
	if err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerWaitRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
	if err = td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err = validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) lifetimeWorkerWaitBinding() (*task.LifetimeRegistrationRecord, string, *task.LifetimeWorkerRecord, string, error) {
	registration, err := td.ReadLifetimeRegistration()
	if err != nil {
		return nil, "", nil, "", err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		return nil, "", nil, "", err
	}
	worker, workerErr := td.ReadLifetimeWorker()
	if errors.Is(workerErr, os.ErrNotExist) {
		return registration, registrationDigest, nil, "", nil
	}
	if workerErr != nil {
		return nil, "", nil, "", workerErr
	}
	workerDigest, err := td.LifetimeWorkerDigest()
	if err != nil {
		return nil, "", nil, "", err
	}
	return registration, registrationDigest, worker, workerDigest, nil
}

func validateLifetimeWorkerWaitForTask(record *task.LifetimeWorkerWaitRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string, worker *task.LifetimeWorkerRecord, workerDigest string) error {
	if err := task.ValidateLifetimeWorkerWaitRecord(record); err != nil {
		return err
	}
	if !task.LifetimeWorkerWaitMatchesRegistration(record, registration, registrationDigest) {
		return task.ErrIdentityMismatch
	}
	if worker == nil {
		if task.LifetimeWorkerWaitProvesNotStarted(record) {
			return nil
		}
		if task.LifetimeWorkerWaitIsQualified(record) || registration.Assurance != task.LifetimeAssuranceExact {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	if !task.LifetimeWorkerWaitIsQualified(record) || !task.LifetimeWorkerWaitMatchesWorker(record, worker, workerDigest) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (td *TaskDir) RecordLifetimeLaunch(facts LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	if facts.Sequence == 0 || facts.Sequence > task.MaxLifetimeLaunches {
		return nil, fmt.Errorf("%w: lifetime launch sequence is outside 1..%d", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
	}
	if _, err := td.ReadLifetimeTermination(); err == nil {
		return nil, task.ErrTerminalTask
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		return nil, err
	}
	record := task.LifetimeLaunchRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		Sequence:           facts.Sequence,
		PID:                facts.PID,
		ProcessStart:       facts.ProcessStart,
		PGID:               facts.PGID,
		StartedAt:          timestamp(),
	}
	if err = task.ValidateLifetimeLaunchRecord(&record); err != nil {
		return nil, err
	}
	name := lifetimeLaunchName(facts.Sequence)
	err = td.recordSame(filepath.Join(lifetimeDirectory, name), record, func() (bool, error) {
		existing, readErr := td.readLifetimeLaunch(facts.Sequence)
		if readErr != nil {
			return false, readErr
		}
		record.StartedAt = existing.StartedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.readLifetimeLaunch(facts.Sequence)
}

func (td *TaskDir) ReadLifetimeLaunches() ([]task.LifetimeLaunchRecord, error) {
	dir, err := td.store.openDir(filepath.Join(td.Dir, lifetimeDirectory))
	if err != nil {
		return nil, err
	}
	var sequences []uint64
	for {
		names, readErr := dir.Readdirnames(1)
		for _, name := range names {
			sequence, launch := parseLifetimeLaunchName(name)
			if launch {
				sequences = append(sequences, sequence)
			}
		}
		if uint64(len(sequences)) > task.MaxLifetimeLaunches {
			err = errors.Join(err, fmt.Errorf("%w: more than %d lifetime launches", task.ErrEvidenceFault, task.MaxLifetimeLaunches))
			break
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			err = errors.Join(err, readErr)
			break
		}
	}
	err = errors.Join(err, dir.Close())
	if err != nil {
		return nil, err
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	launches := make([]task.LifetimeLaunchRecord, 0, len(sequences))
	for index, sequence := range sequences {
		if sequence != uint64(index+1) {
			return nil, task.ErrIdentityMismatch
		}
		launch, readErr := td.readLifetimeLaunch(sequence)
		if readErr != nil {
			return nil, readErr
		}
		launches = append(launches, *launch)
	}
	return launches, nil
}

func (td *TaskDir) RecordLifetimeTermination(cause string) (*task.LifetimeTerminationRecord, error) {
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil {
		return nil, err
	}
	launches, err := td.ReadLifetimeLaunches()
	if err != nil {
		return nil, err
	}
	launchSetDigest, err := lifetimeLaunchSetDigest(launches)
	if err != nil {
		return nil, err
	}
	record := task.LifetimeTerminationRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		LaunchCount:        uint64(len(launches)),
		LaunchSetSHA256:    launchSetDigest,
		Cause:              cause,
		ScopeEmpty:         true,
		ObservedAt:         timestamp(),
	}
	if err = task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return nil, err
	}
	err = td.recordSame(filepath.Join(lifetimeDirectory, lifetimeTerminationName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeTermination()
		if readErr != nil {
			return false, readErr
		}
		record.ObservedAt = existing.ObservedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeTermination()
}

func (td *TaskDir) ReadLifetimeTermination() (*task.LifetimeTerminationRecord, error) {
	var record task.LifetimeTerminationRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)
	if err := td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeIdentity(record.RootID, record.TaskID); err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil || registrationDigest != record.RegistrationSHA256 {
		return nil, errors.Join(task.ErrIdentityMismatch, err)
	}
	launches, err := td.ReadLifetimeLaunches()
	if err != nil {
		return nil, err
	}
	digest, err := lifetimeLaunchSetDigest(launches)
	if err != nil {
		return nil, err
	}
	if uint64(len(launches)) != record.LaunchCount || digest != record.LaunchSetSHA256 {
		return nil, task.ErrIdentityMismatch
	}
	return &record, nil
}

func (td *TaskDir) HasTerminatedLifetime(cause string) (bool, error) {
	record, err := td.ReadLifetimeTermination()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.ScopeEmpty && (cause == "" || record.Cause == cause), nil
}

// StopTerminationReady is the canonical bridge between queue stopping and
// process-lifetime authority. Tasks with a registered native scope may publish
// an ended stop observation only after that exact scope has an empty proof.
// Tasks without a registration retain the queue-only path used before native
// launch and by historical records.
func (td *TaskDir) StopTerminationReady() (bool, error) {
	if _, err := td.ReadLifetimeRegistration(); errors.Is(err, os.ErrNotExist) {
		if _, intentErr := td.ReadLifetimeIntent(); intentErr == nil {
			return false, nil
		} else if errors.Is(intentErr, os.ErrNotExist) {
			return true, nil
		} else {
			return false, intentErr
		}
	} else if err != nil {
		return false, err
	}
	termination, err := td.ReadLifetimeTermination()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if termination.Cause == task.LifetimeTerminationPreviousBoot {
		return true, nil
	}
	if _, err = td.ReadLifetimeWorkerWait(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (td *TaskDir) readLifetimeLaunch(sequence uint64) (*task.LifetimeLaunchRecord, error) {
	var record task.LifetimeLaunchRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(sequence))
	if err := td.store.readRecord(path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeLaunchRecord(&record); err != nil {
		return nil, err
	}
	if record.Sequence != sequence {
		return nil, task.ErrIdentityMismatch
	}
	if err := td.matchLifetimeIdentity(record.RootID, record.TaskID); err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigest()
	if err != nil || registrationDigest != record.RegistrationSHA256 {
		return nil, errors.Join(task.ErrIdentityMismatch, err)
	}
	return &record, nil
}

func (td *TaskDir) matchLifetimeOwner(root, id, ownerKind, ownerSHA string) error {
	_, _, _, expectedSpec, expectedMeta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return err
	}
	expectedOwner, err := task.LifetimeTaskOwnerSHA256(expectedSpec, expectedMeta)
	if err != nil {
		return err
	}
	if root != td.store.RootID || id != td.TaskID || ownerKind != task.LifetimeOwnerTask || ownerSHA != expectedOwner {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (td *TaskDir) matchLifetimeIdentity(root, id string) error {
	if root != td.store.RootID || id != td.TaskID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func lifetimeLaunchName(sequence uint64) string {
	return fmt.Sprintf("%020d%s", sequence, lifetimeLaunchSuffix)
}

func parseLifetimeLaunchName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, lifetimeLaunchSuffix) {
		return 0, false
	}
	prefix := strings.TrimSuffix(name, lifetimeLaunchSuffix)
	if len(prefix) != 20 {
		return 0, false
	}
	sequence, err := strconv.ParseUint(prefix, 10, 64)
	return sequence, err == nil && sequence > 0
}

func lifetimeLaunchSetDigest(launches []task.LifetimeLaunchRecord) (string, error) {
	data, err := task.MarshalCanonical(launches)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}
