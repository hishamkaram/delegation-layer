package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	inspectionLifetimeIntentName       = "lifetime-intent.json"
	inspectionLifetimeRegistrationName = "lifetime-registration.json"
	inspectionLifetimeWorkerName       = "lifetime-worker.json"
	inspectionLifetimeWorkerWaitName   = "lifetime-worker-wait.json"
	inspectionLifetimeTerminationName  = "lifetime-termination.json"
	inspectionLifetimeLaunchPrefix     = "lifetime-launch-"
	inspectionLifetimeLaunchSuffix     = ".json"
)

// AcquireLifetimeOwner retains the control directory's process-scope lease
// for the guardian lifetime. The guardian must release it after termination
// evidence is durable.
func (o *Operation) AcquireLifetimeOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	if o == nil || o.control == nil {
		return nil, task.ErrInvalidPermit
	}
	return o.control.AcquireLifetimeOwner(ctx)
}

// RecordLifetimeIntent persists the inspection-owned scope plan before native
// scope creation.
func (o *Operation) RecordLifetimeIntent(facts taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	return o.RecordLifetimeIntentContext(context.Background(), facts)
}

// RecordLifetimeIntentContext persists the scope plan under the caller's
// bounded control transaction.
func (o *Operation) RecordLifetimeIntentContext(ctx context.Context, facts taskdir.LifetimeIntentFacts) (result *task.LifetimeIntentRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	if facts.Deadline.IsZero() || facts.Deadline.UTC().Format(time.RFC3339Nano) != o.request.Deadline {
		return nil, task.ErrIdentityMismatch
	}
	candidate := task.LifetimeIntentRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        o.request.RootID,
		TaskID:        o.request.TaskID,
		OwnerKind:     task.LifetimeOwnerInspection,
		OwnerSHA256:   o.digest,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopePlan:     facts.ScopePlan,
		BootID:        facts.BootID,
		Deadline:      o.request.Deadline,
		CreatedAt:     canonicalNow(time.Now().UTC()),
	}
	if err := validateLifetimeIntentForOperation(o, candidate); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordLifetimeIntentTx(tx, o, candidate)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeIntentTx(tx *taskdir.ControlTransaction, o *Operation, candidate task.LifetimeIntentRecord) (*task.LifetimeIntentRecord, error) {
	existing, readErr := readLifetimeIntentTx(tx, o)
	if readErr == nil {
		candidate.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	created, putErr := tx.Put(inspectionLifetimeIntentName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readLifetimeIntentTx(tx, o)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.CreatedAt = winner.CreatedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeIntent returns the strictly validated inspection scope plan.
func (o *Operation) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	return o.ReadLifetimeIntentContext(context.Background())
}

// ReadLifetimeIntentContext reads the scope plan under a bounded transaction.
func (o *Operation) ReadLifetimeIntentContext(ctx context.Context) (result *task.LifetimeIntentRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		result, readErr = readLifetimeIntentTx(tx, o)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeRegistration persists the inspection-owned scope binding.
func (o *Operation) RecordLifetimeRegistration(facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	return o.RecordLifetimeRegistrationContext(context.Background(), facts)
}

// RecordLifetimeRegistrationContext persists the inspection-owned scope
// binding while honoring the caller's bounded transaction lifetime.
func (o *Operation) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (result *task.LifetimeRegistrationRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	if facts.Deadline.IsZero() || facts.Deadline.UTC().Format(time.RFC3339Nano) != o.request.Deadline {
		return nil, task.ErrIdentityMismatch
	}
	candidate := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        o.request.RootID,
		TaskID:        o.request.TaskID,
		OwnerKind:     task.LifetimeOwnerInspection,
		OwnerSHA256:   o.digest,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopeID:       facts.ScopeID,
		BootID:        facts.BootID,
		Deadline:      o.request.Deadline,
		CreatedAt:     canonicalNow(time.Now().UTC()),
	}
	if err := validateLifetimeRegistrationForOperation(o, candidate); err != nil {
		return nil, err
	}

	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordLifetimeRegistrationTx(tx, o, candidate)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeRegistrationTx(tx *taskdir.ControlTransaction, o *Operation, candidate task.LifetimeRegistrationRecord) (*task.LifetimeRegistrationRecord, error) {
	existing, _, readErr := readLifetimeRegistrationTx(tx, o)
	if readErr == nil {
		candidate.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	intent, intentErr := readLifetimeIntentTx(tx, o)
	if intentErr != nil {
		return nil, intentErr
	}
	if !task.LifetimeIntentMatchesRegistration(intent, &candidate) {
		return nil, task.ErrIdentityMismatch
	}
	created, putErr := tx.Put(inspectionLifetimeRegistrationName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, _, winnerErr := readLifetimeRegistrationTx(tx, o)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.CreatedAt = winner.CreatedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeRegistration returns the strictly validated inspection scope
// registration, including its canonical winner bytes.
func (o *Operation) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	return o.ReadLifetimeRegistrationContext(context.Background())
}

// ReadLifetimeRegistrationContext reads the registration under the caller's
// bounded transaction lifetime.
func (o *Operation) ReadLifetimeRegistrationContext(ctx context.Context) (result *task.LifetimeRegistrationRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		result, _, readErr = readLifetimeRegistrationTx(tx, o)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// LifetimeRegistrationDigest returns the digest of the canonical persisted
// registration record.
func (o *Operation) LifetimeRegistrationDigest() (string, error) {
	return o.LifetimeRegistrationDigestContext(context.Background())
}

// LifetimeRegistrationDigestContext returns the registration digest under a
// bounded transaction lifetime.
func (o *Operation) LifetimeRegistrationDigestContext(ctx context.Context) (string, error) {
	registration, err := o.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return "", err
	}
	return lifetimeRecordDigest(registration)
}

// RecordLifetimeWorker persists the managed worker identity for the current
// canonical registration.
func (o *Operation) RecordLifetimeWorker(facts taskdir.LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	return o.RecordLifetimeWorkerContext(context.Background(), facts)
}

// RecordLifetimeWorkerContext persists the worker identity under a bounded
// control transaction.
func (o *Operation) RecordLifetimeWorkerContext(ctx context.Context, facts taskdir.LifetimeWorkerFacts) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		registration, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
		if readErr != nil {
			return readErr
		}
		candidate := task.LifetimeWorkerRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             o.request.RootID,
			TaskID:             o.request.TaskID,
			RegistrationSHA256: registrationDigest,
			PID:                facts.PID,
			ProcessStart:       facts.ProcessStart,
			BootID:             facts.BootID,
			StartedAt:          canonicalNow(time.Now().UTC()),
		}
		if err := validateLifetimeWorkerForOperation(o, candidate, registration, registrationDigest); err != nil {
			return err
		}
		result, readErr = recordLifetimeWorkerTx(tx, o, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeWorkerTx(tx *taskdir.ControlTransaction, o *Operation, candidate task.LifetimeWorkerRecord) (*task.LifetimeWorkerRecord, error) {
	existing, readErr := readLifetimeWorkerTx(tx, o)
	if readErr == nil {
		candidate.StartedAt = existing.StartedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	created, putErr := tx.Put(inspectionLifetimeWorkerName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readLifetimeWorkerTx(tx, o)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.StartedAt = winner.StartedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeWorker returns the worker identity bound to the current
// registration.
func (o *Operation) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	return o.ReadLifetimeWorkerContext(context.Background())
}

// ReadLifetimeWorkerContext reads the worker identity under a bounded
// transaction.
func (o *Operation) ReadLifetimeWorkerContext(ctx context.Context) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		result, readErr = readLifetimeWorkerTx(tx, o)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeWorkerWait persists the create-once worker completion
// disposition under the inspection operation's control transaction.
func (o *Operation) RecordLifetimeWorkerWait(facts taskdir.LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	return o.RecordLifetimeWorkerWaitContext(context.Background(), facts)
}

// RecordLifetimeWorkerWaitContext persists a worker completion disposition
// while honoring the caller's bounded operation lifetime.
func (o *Operation) RecordLifetimeWorkerWaitContext(ctx context.Context, facts taskdir.LifetimeWorkerWaitFacts) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	observedAt := facts.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		registration, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
		if readErr != nil {
			return readErr
		}
		worker, workerDigest, workerErr := readLifetimeWorkerBindingTx(tx, o)
		if workerErr != nil {
			return workerErr
		}
		candidate := task.LifetimeWorkerWaitRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             o.request.RootID,
			TaskID:             o.request.TaskID,
			RegistrationSHA256: registrationDigest,
			WorkerSHA256:       facts.WorkerSHA256,
			ChildPID:           facts.ChildPID,
			Result:             facts.Result,
			ExitCode:           facts.ExitCode,
			ObservedAt:         canonicalNow(observedAt),
		}
		if err := validateLifetimeWorkerWaitForOperation(o, candidate, registration, registrationDigest, worker, workerDigest); err != nil {
			return err
		}
		result, readErr = recordLifetimeWorkerWaitTx(tx, o, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeWorkerWaitTx(tx *taskdir.ControlTransaction, o *Operation, candidate task.LifetimeWorkerWaitRecord) (*task.LifetimeWorkerWaitRecord, error) {
	existing, readErr := readLifetimeWorkerWaitTx(tx, o)
	if readErr == nil {
		candidate.ObservedAt = existing.ObservedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	created, putErr := tx.Put(inspectionLifetimeWorkerWaitName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readLifetimeWorkerWaitTx(tx, o)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.ObservedAt = winner.ObservedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (o *Operation) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	return o.ReadLifetimeWorkerWaitContext(context.Background())
}

// ReadLifetimeWorkerWaitContext reads the child Wait winner under a bounded
// control transaction.
func (o *Operation) ReadLifetimeWorkerWaitContext(ctx context.Context) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var readErr error
		result, readErr = readLifetimeWorkerWaitTx(tx, o)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeLaunch persists one create-once native launch bound to the
// current registration. New sequences must extend the contiguous set.
func (o *Operation) RecordLifetimeLaunch(facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	return o.RecordLifetimeLaunchContext(context.Background(), facts)
}

// RecordLifetimeLaunchContext persists one native launch under the caller's
// bounded transaction lifetime.
func (o *Operation) RecordLifetimeLaunchContext(ctx context.Context, facts taskdir.LifetimeLaunchFacts) (result *task.LifetimeLaunchRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	if facts.Sequence == 0 || facts.Sequence > task.MaxLifetimeLaunches {
		return nil, fmt.Errorf("%w: lifetime launch sequence is outside 1..%d", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordLifetimeLaunchTx(tx, o, facts)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeLaunchTx(tx *taskdir.ControlTransaction, o *Operation, facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	_, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
	if readErr != nil {
		return nil, readErr
	}
	launches, launchesErr := readLifetimeLaunchesTx(tx, o, registrationDigest)
	if launchesErr != nil {
		return nil, launchesErr
	}
	if _, terminationErr := readLifetimeTerminationTx(tx, o, registrationDigest, launches); terminationErr == nil {
		return nil, task.ErrTerminalTask
	} else if !errors.Is(terminationErr, os.ErrNotExist) {
		return nil, terminationErr
	}

	if facts.Sequence <= uint64(len(launches)) {
		existing := launches[facts.Sequence-1]
		candidate := lifetimeLaunchRecord(o, facts, registrationDigest, existing.StartedAt)
		if err := validateLifetimeLaunchForOperation(o, candidate, registrationDigest); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return &existing, nil
	}
	if facts.Sequence != uint64(len(launches)+1) {
		return nil, task.ErrIdentityMismatch
	}

	candidate := lifetimeLaunchRecord(o, facts, registrationDigest, canonicalNow(time.Now().UTC()))
	if err := validateLifetimeLaunchForOperation(o, candidate, registrationDigest); err != nil {
		return nil, err
	}
	created, putErr := tx.Put(inspectionLifetimeLaunchName(facts.Sequence), candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winnerLaunches, winnerErr := readLifetimeLaunchesTx(tx, o, registrationDigest)
	if winnerErr != nil {
		return nil, winnerErr
	}
	if facts.Sequence > uint64(len(winnerLaunches)) {
		return nil, task.ErrIdentityMismatch
	}
	winner := winnerLaunches[facts.Sequence-1]
	if !reflect.DeepEqual(winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return &winner, nil
}

// ReadLifetimeLaunches probes the bounded canonical sequence namespace and
// rejects gaps, malformed winners, conflicting identities, and overflow.
func (o *Operation) ReadLifetimeLaunches() ([]task.LifetimeLaunchRecord, error) {
	return o.ReadLifetimeLaunchesContext(context.Background())
}

// ReadLifetimeLaunchesContext reads all contiguous launch records under the
// caller's bounded transaction lifetime.
func (o *Operation) ReadLifetimeLaunchesContext(ctx context.Context) (result []task.LifetimeLaunchRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		_, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
		if readErr != nil {
			return readErr
		}
		result, readErr = readLifetimeLaunchesTx(tx, o, registrationDigest)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeTermination persists the guardian's independently proven empty
// scope observation and fences all later launches.
func (o *Operation) RecordLifetimeTermination(cause string) (*task.LifetimeTerminationRecord, error) {
	return o.RecordLifetimeTerminationContext(context.Background(), cause)
}

// RecordLifetimeTerminationContext persists termination under the caller's
// bounded transaction lifetime. ScopeEmpty is set only at this guardian-owned
// boundary, after the caller has independently proved the scope is empty.
func (o *Operation) RecordLifetimeTerminationContext(ctx context.Context, cause string) (result *task.LifetimeTerminationRecord, resultErr error) {
	if err := o.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordLifetimeTerminationTx(tx, o, cause)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordLifetimeTerminationTx(tx *taskdir.ControlTransaction, o *Operation, cause string) (*task.LifetimeTerminationRecord, error) {
	_, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
	if readErr != nil {
		return nil, readErr
	}
	launches, launchesErr := readLifetimeLaunchesTx(tx, o, registrationDigest)
	if launchesErr != nil {
		return nil, launchesErr
	}
	launchSetDigest, digestErr := lifetimeLaunchSetDigest(launches)
	if digestErr != nil {
		return nil, digestErr
	}
	candidate := task.LifetimeTerminationRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             o.request.RootID,
		TaskID:             o.request.TaskID,
		RegistrationSHA256: registrationDigest,
		LaunchCount:        uint64(len(launches)),
		LaunchSetSHA256:    launchSetDigest,
		Cause:              cause,
		ScopeEmpty:         true,
		ObservedAt:         canonicalNow(time.Now().UTC()),
	}
	if err := validateLifetimeTerminationForOperation(o, candidate, registrationDigest, launches); err != nil {
		return nil, err
	}

	existing, terminationErr := readLifetimeTerminationTx(tx, o, registrationDigest, launches)
	if terminationErr == nil {
		candidate.ObservedAt = existing.ObservedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !errors.Is(terminationErr, os.ErrNotExist) {
		return nil, terminationErr
	}

	created, putErr := tx.Put(inspectionLifetimeTerminationName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readLifetimeTerminationTx(tx, o, registrationDigest, launches)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.ObservedAt = winner.ObservedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeTermination returns the strict create-once termination winner.
func (o *Operation) ReadLifetimeTermination() (*task.LifetimeTerminationRecord, error) {
	return o.ReadLifetimeTerminationContext(context.Background())
}

// ReadLifetimeTerminationContext validates termination against the complete
// canonical launch set under the caller's bounded transaction lifetime.
func (o *Operation) ReadLifetimeTerminationContext(ctx context.Context) (result *task.LifetimeTerminationRecord, resultErr error) {
	if err := o.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		_, registrationDigest, readErr := readLifetimeRegistrationTx(tx, o)
		if readErr != nil {
			return readErr
		}
		launches, launchesErr := readLifetimeLaunchesTx(tx, o, registrationDigest)
		if launchesErr != nil {
			return launchesErr
		}
		result, readErr = readLifetimeTerminationTx(tx, o, registrationDigest, launches)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// HasTerminatedLifetime reports whether a strictly validated empty-scope
// termination exists, optionally for one cause.
func (o *Operation) HasTerminatedLifetime(cause string) (bool, error) {
	return o.HasTerminatedLifetimeContext(context.Background(), cause)
}

// HasTerminatedLifetimeContext reports termination under a bounded read.
func (o *Operation) HasTerminatedLifetimeContext(ctx context.Context, cause string) (bool, error) {
	record, err := o.ReadLifetimeTerminationContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.ScopeEmpty && (cause == "" || record.Cause == cause), nil
}

func (o *Operation) validateLifetimeOperation(writing bool) error {
	if o == nil || o.control == nil {
		if writing {
			return task.ErrEvidenceFault
		}
		return os.ErrClosed
	}
	if err := task.ValidateRootID(o.request.RootID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(o.request.TaskID); err != nil {
		return err
	}
	if err := task.ValidateSHA256(o.digest); err != nil {
		return err
	}
	deadline, err := canonicalTimestamp("inspection lifetime deadline", o.request.Deadline)
	if err != nil {
		return err
	}
	if o.deadline.IsZero() || !o.deadline.Equal(deadline) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeRegistrationForOperation(o *Operation, record task.LifetimeRegistrationRecord) error {
	if err := task.ValidateLifetimeRegistrationRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || record.OwnerKind != task.LifetimeOwnerInspection || record.OwnerSHA256 != o.digest || record.Deadline != o.request.Deadline {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeIntentForOperation(o *Operation, record task.LifetimeIntentRecord) error {
	if err := task.ValidateLifetimeIntentRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || record.OwnerKind != task.LifetimeOwnerInspection || record.OwnerSHA256 != o.digest || record.Deadline != o.request.Deadline {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeWorkerForOperation(o *Operation, record task.LifetimeWorkerRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string) error {
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || record.RegistrationSHA256 != registrationDigest || registration == nil || record.BootID != registration.BootID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeWorkerWaitForOperation(o *Operation, record task.LifetimeWorkerWaitRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string, worker *task.LifetimeWorkerRecord, workerDigest string) error {
	if err := task.ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || !task.LifetimeWorkerWaitMatchesRegistration(&record, registration, registrationDigest) {
		return task.ErrIdentityMismatch
	}
	if worker == nil {
		if task.LifetimeWorkerWaitProvesNotStarted(&record) {
			return nil
		}
		if task.LifetimeWorkerWaitIsQualified(&record) || registration.Assurance != task.LifetimeAssuranceExact {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	if !task.LifetimeWorkerWaitIsQualified(&record) || !task.LifetimeWorkerWaitMatchesWorker(&record, worker, workerDigest) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeLaunchForOperation(o *Operation, record task.LifetimeLaunchRecord, registrationDigest string) error {
	if err := task.ValidateLifetimeLaunchRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || record.RegistrationSHA256 != registrationDigest || record.Sequence == 0 || record.Sequence > task.MaxLifetimeLaunches {
		return task.ErrIdentityMismatch
	}
	started, err := canonicalTimestamp("inspection lifetime launch", record.StartedAt)
	if err != nil {
		return err
	}
	deadline, err := canonicalTimestamp("inspection lifetime deadline", o.request.Deadline)
	if err != nil || !started.Before(deadline) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateLifetimeTerminationForOperation(o *Operation, record task.LifetimeTerminationRecord, registrationDigest string, launches []task.LifetimeLaunchRecord) error {
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return err
	}
	if record.RootID != o.request.RootID || record.TaskID != o.request.TaskID || record.RegistrationSHA256 != registrationDigest || record.LaunchCount != uint64(len(launches)) {
		return task.ErrIdentityMismatch
	}
	digest, err := lifetimeLaunchSetDigest(launches)
	if err != nil {
		return err
	}
	if record.LaunchSetSHA256 != digest {
		return task.ErrIdentityMismatch
	}
	return nil
}

func readLifetimeRegistrationTx(tx *taskdir.ControlTransaction, o *Operation) (*task.LifetimeRegistrationRecord, string, error) {
	var record task.LifetimeRegistrationRecord
	if err := tx.Read(inspectionLifetimeRegistrationName, &record); err != nil {
		return nil, "", lifetimeReadError(inspectionLifetimeRegistrationName, err)
	}
	if err := validateLifetimeRegistrationForOperation(o, record); err != nil {
		return nil, "", err
	}
	if err := tx.Verify(inspectionLifetimeRegistrationName, record); err != nil {
		return nil, "", err
	}
	digest, err := lifetimeRecordDigest(record)
	if err != nil {
		return nil, "", err
	}
	return &record, digest, nil
}

func readLifetimeIntentTx(tx *taskdir.ControlTransaction, o *Operation) (*task.LifetimeIntentRecord, error) {
	var record task.LifetimeIntentRecord
	if err := tx.Read(inspectionLifetimeIntentName, &record); err != nil {
		return nil, lifetimeReadError(inspectionLifetimeIntentName, err)
	}
	if err := validateLifetimeIntentForOperation(o, record); err != nil {
		return nil, err
	}
	if err := tx.Verify(inspectionLifetimeIntentName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readLifetimeWorkerTx(tx *taskdir.ControlTransaction, o *Operation) (*task.LifetimeWorkerRecord, error) {
	registration, registrationDigest, err := readLifetimeRegistrationTx(tx, o)
	if err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerRecord
	if err = tx.Read(inspectionLifetimeWorkerName, &record); err != nil {
		return nil, lifetimeReadError(inspectionLifetimeWorkerName, err)
	}
	if err = validateLifetimeWorkerForOperation(o, record, registration, registrationDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(inspectionLifetimeWorkerName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readLifetimeWorkerBindingTx(tx *taskdir.ControlTransaction, o *Operation) (*task.LifetimeWorkerRecord, string, error) {
	worker, err := readLifetimeWorkerTx(tx, o)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	digest, err := lifetimeRecordDigest(worker)
	if err != nil {
		return nil, "", err
	}
	return worker, digest, nil
}

func readLifetimeWorkerWaitTx(tx *taskdir.ControlTransaction, o *Operation) (*task.LifetimeWorkerWaitRecord, error) {
	registration, registrationDigest, err := readLifetimeRegistrationTx(tx, o)
	if err != nil {
		return nil, err
	}
	worker, workerDigest, err := readLifetimeWorkerBindingTx(tx, o)
	if err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerWaitRecord
	if err = tx.Read(inspectionLifetimeWorkerWaitName, &record); err != nil {
		return nil, lifetimeReadError(inspectionLifetimeWorkerWaitName, err)
	}
	if err = validateLifetimeWorkerWaitForOperation(o, record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(inspectionLifetimeWorkerWaitName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readLifetimeLaunchesTx(tx *taskdir.ControlTransaction, o *Operation, registrationDigest string) ([]task.LifetimeLaunchRecord, error) {
	launches := make([]task.LifetimeLaunchRecord, 0, task.MaxLifetimeLaunches)
	missing := false
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches+1; sequence++ {
		name := inspectionLifetimeLaunchName(sequence)
		var record task.LifetimeLaunchRecord
		err := tx.Read(name, &record)
		if errors.Is(err, os.ErrNotExist) {
			if sequence <= task.MaxLifetimeLaunches {
				missing = true
			}
			continue
		}
		if err != nil {
			return nil, lifetimeReadError(name, err)
		}
		if sequence > task.MaxLifetimeLaunches {
			return nil, fmt.Errorf("%w: more than %d lifetime launches", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
		}
		if missing {
			return nil, task.ErrIdentityMismatch
		}
		if err := validateLifetimeLaunchForOperation(o, record, registrationDigest); err != nil {
			return nil, err
		}
		if record.Sequence != sequence {
			return nil, task.ErrIdentityMismatch
		}
		if err := tx.Verify(name, record); err != nil {
			return nil, err
		}
		launches = append(launches, record)
	}
	return launches, nil
}

func readLifetimeTerminationTx(tx *taskdir.ControlTransaction, o *Operation, registrationDigest string, launches []task.LifetimeLaunchRecord) (*task.LifetimeTerminationRecord, error) {
	var record task.LifetimeTerminationRecord
	if err := tx.Read(inspectionLifetimeTerminationName, &record); err != nil {
		return nil, lifetimeReadError(inspectionLifetimeTerminationName, err)
	}
	if err := validateLifetimeTerminationForOperation(o, record, registrationDigest, launches); err != nil {
		return nil, err
	}
	if err := tx.Verify(inspectionLifetimeTerminationName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func lifetimeLaunchRecord(o *Operation, facts taskdir.LifetimeLaunchFacts, registrationDigest, startedAt string) task.LifetimeLaunchRecord {
	return task.LifetimeLaunchRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             o.request.RootID,
		TaskID:             o.request.TaskID,
		RegistrationSHA256: registrationDigest,
		Sequence:           facts.Sequence,
		PID:                facts.PID,
		ProcessStart:       facts.ProcessStart,
		PGID:               facts.PGID,
		StartedAt:          startedAt,
	}
}

func lifetimeLaunchSetDigest(launches []task.LifetimeLaunchRecord) (string, error) {
	data, err := task.MarshalCanonical(launches)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func lifetimeRecordDigest(record any) (string, error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func lifetimeReadError(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: invalid persisted %s: %w", task.ErrEvidenceFault, name, err)
}

func inspectionLifetimeLaunchName(sequence uint64) string {
	return fmt.Sprintf("%s%020d%s", inspectionLifetimeLaunchPrefix, sequence, inspectionLifetimeLaunchSuffix)
}
