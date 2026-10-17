package taskdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
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
)

func (td *TaskDir) RecordLifetimeIntent(facts LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	return td.RecordLifetimeIntentContext(context.Background(), facts)
}

// RecordLifetimeIntentContext persists the lifetime scope plan under the
// caller's bounded persistence context.
func (td *TaskDir) RecordLifetimeIntentContext(ctx context.Context, facts LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err = td.requireTaskLaunchPermissionContext(ctx); err != nil {
		return nil, err
	}

	if err = td.rejectRecoveredLifetimeLaunchContext(ctx); err != nil {
		return nil, err
	}

	_, _, _, spec, meta, err := td.loadAndValidatePreparedSetContext(ctx)
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
	if err = td.store.mkdirContext(ctx, filepath.Join(td.Dir, lifetimeDirectory), td.store.faultInjector); err != nil {
		return nil, err
	}
	err = td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeIntentName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeIntentContext(ctx)
		if readErr != nil {
			return false, readErr
		}
		record.CreatedAt = existing.CreatedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeIntentContext(ctx)
}

func (td *TaskDir) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	return td.ReadLifetimeIntentContext(context.Background())
}

// ReadLifetimeIntentContext validates the intent against its admitted task
// while preserving the caller's context through every nested read.
func (td *TaskDir) ReadLifetimeIntentContext(ctx context.Context) (*task.LifetimeIntentRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeIntentRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeIntentName)
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeIntentRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeOwner(ctx, record.RootID, record.TaskID, record.OwnerKind, record.OwnerSHA256); err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	return &record, nil
}

func (td *TaskDir) RecordLifetimeRegistration(facts LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	return td.RecordLifetimeRegistrationContext(context.Background(), facts)
}

// RecordLifetimeRegistrationContext persists the scope registration under the
// caller's bounded persistence context.
func (td *TaskDir) RecordLifetimeRegistrationContext(ctx context.Context, facts LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err = td.rejectRecoveredLifetimeLaunchContext(ctx); err != nil {
		return nil, err
	}

	_, _, _, spec, meta, err := td.loadAndValidatePreparedSetContext(ctx)
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
	if existing, readErr := td.ReadLifetimeRegistrationContext(ctx); readErr == nil {
		record.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(existing, &record) {
			return nil, task.ErrIdentityMismatch
		}
		if acknowledgeErr := td.acknowledgeSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeRegistrationName), true); acknowledgeErr != nil {
			return nil, acknowledgeErr
		}
		return td.ReadLifetimeRegistrationContext(ctx)
	} else if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	intent, err := td.ReadLifetimeIntentContext(ctx)
	if err != nil {
		return nil, err
	}
	if !task.LifetimeIntentMatchesRegistration(intent, &record) {
		return nil, task.ErrIdentityMismatch
	}
	err = td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeRegistrationName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeRegistrationContext(ctx)
		if readErr != nil {
			return false, readErr
		}
		record.CreatedAt = existing.CreatedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeRegistrationContext(ctx)
}

func (td *TaskDir) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	return td.ReadLifetimeRegistrationContext(context.Background())
}

// ReadLifetimeRegistrationContext reads the registration while preserving the
// caller's cancellation and deadline at each persistence boundary.
func (td *TaskDir) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeRegistrationRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName)
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeRegistrationRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeOwner(ctx, record.RootID, record.TaskID, record.OwnerKind, record.OwnerSHA256); err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) LifetimeRegistrationDigest() (string, error) {
	return td.LifetimeRegistrationDigestContext(context.Background())
}

// LifetimeRegistrationDigestContext returns the persisted registration digest
// while preserving the caller's cancellation and deadline.
func (td *TaskDir) LifetimeRegistrationDigestContext(ctx context.Context) (string, error) {
	if _, err := td.ReadLifetimeRegistrationContext(ctx); err != nil {
		return "", err
	}
	data, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName), task.MaxControlRecordSize)
	if err != nil {
		return "", lifetimeValidationDependencyError(err)
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func (td *TaskDir) RecordLifetimeLaunch(facts LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	return td.RecordLifetimeLaunchContext(context.Background(), facts)
}

// RecordLifetimeLaunchContext persists one native launch while retaining the
// guardian's bounded work context for every nested read and write.
func (td *TaskDir) RecordLifetimeLaunchContext(ctx context.Context, facts LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	if facts.Sequence == 0 || facts.Sequence > task.MaxLifetimeLaunches {
		return nil, fmt.Errorf("%w: lifetime launch sequence is outside 1..%d", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
	}
	if _, err := td.ReadLifetimeTerminationContext(ctx); err == nil {
		return nil, task.ErrTerminalTask
	} else if !task.RecordAbsent(err) {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
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
	err = td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, name), record, func() (bool, error) {
		existing, readErr := td.readLifetimeLaunchContext(ctx, facts.Sequence)
		if readErr != nil {
			return false, readErr
		}
		record.StartedAt = existing.StartedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.readLifetimeLaunchContext(ctx, facts.Sequence)
}

func (td *TaskDir) ReadLifetimeLaunches() ([]task.LifetimeLaunchRecord, error) {
	return td.ReadLifetimeLaunchesContext(context.Background())
}

// ReadLifetimeLaunchesContext reads the complete contiguous launch set while
// retaining the caller's cancellation and deadline for each record read.
func (td *TaskDir) ReadLifetimeLaunchesContext(ctx context.Context) ([]task.LifetimeLaunchRecord, error) {
	sequences, err := td.readLifetimeLaunchSequencesContext(ctx)
	if err != nil {
		return nil, err
	}
	launches := make([]task.LifetimeLaunchRecord, 0, len(sequences))
	for index, sequence := range sequences {
		if err := lifetimeContextErr(ctx); err != nil {
			return nil, err
		}
		if sequence != uint64(index+1) {
			return nil, task.ErrIdentityMismatch
		}
		launch, readErr := td.readLifetimeLaunchContext(ctx, sequence)
		if readErr != nil {
			return nil, readErr
		}
		launches = append(launches, *launch)
	}
	return launches, nil
}

func (td *TaskDir) readLifetimeLaunchSequencesContext(ctx context.Context) ([]uint64, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	dir, err := td.store.openDir(filepath.Join(td.Dir, lifetimeDirectory))
	if err != nil {
		return nil, err
	}
	var sequences []uint64
	for {
		if contextErr := lifetimeContextErr(ctx); contextErr != nil {
			err = contextErr
			break
		}
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
	err = errors.Join(err, dir.Close(), lifetimeContextErr(ctx))
	if err != nil {
		return nil, err
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	return sequences, nil
}

func (td *TaskDir) RecordLifetimeTermination(cause string) (*task.LifetimeTerminationRecord, error) {
	return td.RecordLifetimeTerminationContext(context.Background(), cause)
}

// RecordLifetimeTerminationContext persists the caller's independently proven
// empty-scope observation within its bounded persistence context. Once staging
// starts, the durable writer retains ownership through barriers and cleanup.
func (td *TaskDir) RecordLifetimeTerminationContext(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, err
	}
	launches, err := td.ReadLifetimeLaunchesContext(ctx)
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
	err = td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeTerminationName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeTerminationContext(ctx)
		if readErr != nil {
			return false, readErr
		}
		record.ObservedAt = existing.ObservedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeTerminationContext(ctx)
}

func (td *TaskDir) ReadLifetimeTermination() (*task.LifetimeTerminationRecord, error) {
	return td.ReadLifetimeTerminationContext(context.Background())
}

// ReadLifetimeTerminationContext validates termination against the complete
// launch set while preserving the caller's context for nested reads.
func (td *TaskDir) ReadLifetimeTerminationContext(ctx context.Context) (*task.LifetimeTerminationRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeTerminationRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return nil, err
	}
	if err := td.matchLifetimeIdentity(record.RootID, record.TaskID); err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if registrationDigest != record.RegistrationSHA256 {
		return nil, task.ErrIdentityMismatch
	}
	launches, err := td.ReadLifetimeLaunchesContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	digest, err := lifetimeLaunchSetDigest(launches)
	if err != nil {
		return nil, err
	}
	if uint64(len(launches)) != record.LaunchCount || digest != record.LaunchSetSHA256 {
		return nil, task.ErrIdentityMismatch
	}
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) HasTerminatedLifetime(cause string) (bool, error) {
	return td.HasTerminatedLifetimeContext(context.Background(), cause)
}

// HasTerminatedLifetimeContext checks the existing empty-scope proof without
// detaching its validation from the caller's context.
func (td *TaskDir) HasTerminatedLifetimeContext(ctx context.Context, cause string) (bool, error) {
	record, err := td.ReadLifetimeTerminationContext(ctx)
	if task.RecordAbsent(err) {
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
	return td.StopTerminationReadyContext(context.Background())
}

// StopTerminationReadyContext applies the same stop-termination policy using
// the caller's context for every registration, intent, termination, and Wait read.
func (td *TaskDir) StopTerminationReadyContext(ctx context.Context) (bool, error) {
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return false, err
	}
	if evidence.Intent == nil && evidence.Registration == nil {
		return true, nil
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	return closure.Closed(), err
}

func (td *TaskDir) readLifetimeLaunchContext(ctx context.Context, sequence uint64) (*task.LifetimeLaunchRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeLaunchRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeLaunchName(sequence))
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := lifetimeContextErr(ctx); err != nil {
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
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if registrationDigest != record.RegistrationSHA256 {
		return nil, task.ErrIdentityMismatch
	}
	return &record, nil
}

func lifetimeContextErr(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	return ctx.Err()
}

func lifetimeValidationDependencyError(err error) error {
	if err == nil ||
		errors.Is(err, task.ErrIdentityMismatch) ||
		errors.Is(err, task.ErrEvidenceFault) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(task.ErrIdentityMismatch, err)
}

func (td *TaskDir) matchLifetimeOwner(ctx context.Context, root, id, ownerKind, ownerSHA string) error {
	_, _, _, expectedSpec, expectedMeta, err := td.loadAndValidatePreparedSetContext(ctx)
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
