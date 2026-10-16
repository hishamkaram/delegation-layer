package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	controlLifetimeIntentName       = "lifetime-intent.json"
	controlLifetimeRegistrationName = "lifetime-registration.json"
	controlLifetimeWorkerName       = "lifetime-worker.json"
	controlLifetimeWorkerWaitName   = "lifetime-worker-wait.json"
	controlLifetimeTerminationName  = "lifetime-termination.json"
	controlLifetimeLaunchPrefix     = "lifetime-launch-"
	controlLifetimeLaunchSuffix     = ".json"
)

// ControlLifetime owns one control-directory lifetime record family. The
// operation ID is persisted in the shared TaskID field so the record schema
// remains byte-compatible with ordinary task and inspection lifetime records.
type ControlLifetime struct {
	control     *ControlDir
	rootID      string
	operationID string
	ownerKind   string
	ownerSHA256 string
	deadline    time.Time
}

// NewControlLifetime binds a generic lifetime record family to one control
// directory and its immutable owner identity.
func NewControlLifetime(control *ControlDir, rootID, operationID, ownerKind, ownerSHA256 string, deadline time.Time) (*ControlLifetime, error) {
	if control == nil {
		return nil, task.ErrInvalidPermit
	}
	lifetime := &ControlLifetime{
		control:     control,
		rootID:      rootID,
		operationID: operationID,
		ownerKind:   ownerKind,
		ownerSHA256: ownerSHA256,
		deadline:    deadline,
	}
	if err := lifetime.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	return lifetime, nil
}

// AcquireLifetimeOwner retains the control directory's process-scope lease
// for the guardian lifetime.
func (l *ControlLifetime) AcquireLifetimeOwner(ctx context.Context) (*LifetimeOwnerLease, error) {
	if l == nil || l.control == nil {
		return nil, task.ErrInvalidPermit
	}
	return l.control.AcquireLifetimeOwner(ctx)
}

// RecordLifetimeIntent persists the deterministic scope plan before native
// scope creation.
func (l *ControlLifetime) RecordLifetimeIntent(facts LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	return l.RecordLifetimeIntentContext(context.Background(), facts)
}

// RecordLifetimeIntentContext persists the scope plan under the caller's
// bounded control transaction.
func (l *ControlLifetime) RecordLifetimeIntentContext(ctx context.Context, facts LifetimeIntentFacts) (result *task.LifetimeIntentRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	deadline := l.deadlineText()
	if facts.Deadline.IsZero() || facts.Deadline.UTC().Format(time.RFC3339Nano) != deadline {
		return nil, task.ErrIdentityMismatch
	}
	candidate := task.LifetimeIntentRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        l.rootID,
		TaskID:        l.operationID,
		OwnerKind:     l.ownerKind,
		OwnerSHA256:   l.ownerSHA256,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopePlan:     facts.ScopePlan,
		BootID:        facts.BootID,
		Deadline:      deadline,
		CreatedAt:     timestamp(),
	}
	if err := validateControlLifetimeIntent(l, candidate); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordControlLifetimeIntentTx(tx, l, candidate)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeIntentTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeIntentRecord) (*task.LifetimeIntentRecord, error) {
	existing, readErr := readControlLifetimeIntentTx(tx, l)
	if readErr == nil {
		if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
			return nil, err
		}
		candidate.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
		return nil, err
	}
	if err := ensureNoNativeLifetimeRecordsTx(tx, l); err != nil {
		return nil, err
	}
	created, putErr := tx.Put(controlLifetimeIntentName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeIntentTx(tx, l)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.CreatedAt = winner.CreatedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeIntent returns the strictly validated scope plan.
func (l *ControlLifetime) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	return l.ReadLifetimeIntentContext(context.Background())
}

// ReadLifetimeIntentContext reads the scope plan under a bounded transaction.
func (l *ControlLifetime) ReadLifetimeIntentContext(ctx context.Context) (result *task.LifetimeIntentRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeIntentTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeRegistration persists the scope binding.
func (l *ControlLifetime) RecordLifetimeRegistration(facts LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	return l.RecordLifetimeRegistrationContext(context.Background(), facts)
}

// RecordLifetimeRegistrationContext persists the scope binding under the
// caller's bounded control transaction.
func (l *ControlLifetime) RecordLifetimeRegistrationContext(ctx context.Context, facts LifetimeRegistrationFacts) (result *task.LifetimeRegistrationRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	deadline := l.deadlineText()
	if facts.Deadline.IsZero() || facts.Deadline.UTC().Format(time.RFC3339Nano) != deadline {
		return nil, task.ErrIdentityMismatch
	}
	candidate := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        l.rootID,
		TaskID:        l.operationID,
		OwnerKind:     l.ownerKind,
		OwnerSHA256:   l.ownerSHA256,
		ScopeKind:     facts.ScopeKind,
		Assurance:     facts.Assurance,
		ScopeID:       facts.ScopeID,
		BootID:        facts.BootID,
		Deadline:      deadline,
		CreatedAt:     timestamp(),
	}
	if err := validateControlLifetimeRegistration(l, candidate); err != nil {
		return nil, err
	}

	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordControlLifetimeRegistrationTx(tx, l, candidate)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeRegistrationTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeRegistrationRecord) (*task.LifetimeRegistrationRecord, error) {
	if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
		return nil, err
	}
	existing, _, readErr := readControlLifetimeRegistrationTx(tx, l)
	if readErr == nil {
		candidate.CreatedAt = existing.CreatedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	intent, intentErr := readControlLifetimeIntentTx(tx, l)
	if intentErr != nil {
		return nil, intentErr
	}
	if !task.LifetimeIntentMatchesRegistration(intent, &candidate) {
		return nil, task.ErrIdentityMismatch
	}
	created, putErr := tx.Put(controlLifetimeRegistrationName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, _, winnerErr := readControlLifetimeRegistrationTx(tx, l)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.CreatedAt = winner.CreatedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeRegistration returns the strictly validated scope registration,
// including its canonical winner bytes.
func (l *ControlLifetime) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	return l.ReadLifetimeRegistrationContext(context.Background())
}

// ReadLifetimeRegistrationContext reads the registration under a bounded
// transaction.
func (l *ControlLifetime) ReadLifetimeRegistrationContext(ctx context.Context) (result *task.LifetimeRegistrationRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, _, readErr = readControlLifetimeRegistrationTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// LifetimeRegistrationDigest returns the digest of the canonical persisted
// registration record.
func (l *ControlLifetime) LifetimeRegistrationDigest() (string, error) {
	return l.LifetimeRegistrationDigestContext(context.Background())
}

// LifetimeRegistrationDigestContext returns the registration digest under a
// bounded transaction lifetime.
func (l *ControlLifetime) LifetimeRegistrationDigestContext(ctx context.Context) (string, error) {
	registration, err := l.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return "", err
	}
	return controlLifetimeRecordDigest(registration)
}

// RecordLifetimeWorker persists the managed worker identity for the current
// canonical registration.
func (l *ControlLifetime) RecordLifetimeWorker(facts LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	return l.RecordLifetimeWorkerContext(context.Background(), facts)
}

// RecordLifetimeWorkerContext persists the worker identity under a bounded
// control transaction.
func (l *ControlLifetime) RecordLifetimeWorkerContext(ctx context.Context, facts LifetimeWorkerFacts) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
			return err
		}

		registration, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
		if readErr != nil {
			return readErr
		}
		candidate := task.LifetimeWorkerRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             l.rootID,
			TaskID:             l.operationID,
			RegistrationSHA256: registrationDigest,
			PID:                facts.PID,
			ProcessStart:       facts.ProcessStart,
			BootID:             facts.BootID,
			StartedAt:          timestamp(),
		}
		if err := validateControlLifetimeWorker(l, candidate, registration, registrationDigest); err != nil {
			return err
		}
		result, readErr = recordControlLifetimeWorkerTx(tx, l, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeWorkerTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeWorkerRecord) (*task.LifetimeWorkerRecord, error) {
	existing, readErr := readControlLifetimeWorkerTx(tx, l)
	if readErr == nil {
		candidate.StartedAt = existing.StartedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	created, putErr := tx.Put(controlLifetimeWorkerName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeWorkerTx(tx, l)
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
func (l *ControlLifetime) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	return l.ReadLifetimeWorkerContext(context.Background())
}

// ReadLifetimeWorkerContext reads the worker identity under a bounded
// transaction.
func (l *ControlLifetime) ReadLifetimeWorkerContext(ctx context.Context) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeWorkerTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeWorkerWait persists the create-once worker completion
// disposition.
func (l *ControlLifetime) RecordLifetimeWorkerWait(facts LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	return l.RecordLifetimeWorkerWaitContext(context.Background(), facts)
}

// RecordLifetimeWorkerWaitContext persists a worker completion disposition
// while honoring the caller's bounded lifetime.
func (l *ControlLifetime) RecordLifetimeWorkerWaitContext(ctx context.Context, facts LifetimeWorkerWaitFacts) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	observedAt := facts.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		registration, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
		if readErr != nil {
			return readErr
		}
		worker, workerDigest, workerErr := readControlLifetimeWorkerBindingTx(tx, l)
		if workerErr != nil {
			return workerErr
		}
		candidate := task.LifetimeWorkerWaitRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             l.rootID,
			TaskID:             l.operationID,
			RegistrationSHA256: registrationDigest,
			WorkerSHA256:       facts.WorkerSHA256,
			ChildPID:           facts.ChildPID,
			Result:             facts.Result,
			ExitCode:           facts.ExitCode,
			ObservedAt:         observedAt.UTC().Format(time.RFC3339Nano),
		}
		if err := validateControlLifetimeWorkerWait(l, candidate, registration, registrationDigest, worker, workerDigest); err != nil {
			return err
		}
		result, readErr = recordControlLifetimeWorkerWaitTx(tx, l, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeWorkerWaitTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeWorkerWaitRecord) (*task.LifetimeWorkerWaitRecord, error) {
	existing, readErr := readControlLifetimeWorkerWaitTx(tx, l)
	if readErr == nil {
		candidate.ObservedAt = existing.ObservedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	created, putErr := tx.Put(controlLifetimeWorkerWaitName, candidate)
	if created && putErr == nil {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeWorkerWaitTx(tx, l)
	if winnerErr != nil {
		return nil, errors.Join(putErr, winnerErr)
	}
	candidate.ObservedAt = winner.ObservedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, errors.Join(putErr, task.ErrIdentityMismatch)
	}
	return winner, putErr
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (l *ControlLifetime) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	return l.ReadLifetimeWorkerWaitContext(context.Background())
}

// ReadLifetimeWorkerWaitContext reads the child Wait winner under a bounded
// control transaction.
func (l *ControlLifetime) ReadLifetimeWorkerWaitContext(ctx context.Context) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeWorkerWaitTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeLaunch persists one create-once native launch bound to the
// current registration. New sequences must extend the contiguous set.
func (l *ControlLifetime) RecordLifetimeLaunch(facts LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	return l.RecordLifetimeLaunchContext(context.Background(), facts)
}

// RecordLifetimeLaunchContext persists one native launch under the caller's
// bounded transaction lifetime.
func (l *ControlLifetime) RecordLifetimeLaunchContext(ctx context.Context, facts LifetimeLaunchFacts) (result *task.LifetimeLaunchRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	if facts.Sequence == 0 || facts.Sequence > task.MaxLifetimeLaunches {
		return nil, fmt.Errorf("%w: lifetime launch sequence is outside 1..%d", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordControlLifetimeLaunchTx(tx, l, facts)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeLaunchTx(tx *ControlTransaction, l *ControlLifetime, facts LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	_, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
	if readErr != nil {
		return nil, readErr
	}
	launches, launchesErr := readControlLifetimeLaunchesTx(tx, l, registrationDigest)
	if launchesErr != nil {
		return nil, launchesErr
	}
	if _, terminationErr := readControlLifetimeTerminationTx(tx, l, registrationDigest, launches); terminationErr == nil {
		return nil, task.ErrTerminalTask
	} else if !task.RecordAbsent(terminationErr) {
		return nil, terminationErr
	}

	if facts.Sequence <= uint64(len(launches)) {
		existing := launches[facts.Sequence-1]
		candidate := controlLifetimeLaunchRecord(l, facts, registrationDigest, existing.StartedAt)
		if err := validateControlLifetimeLaunch(l, candidate, registrationDigest); err != nil {
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

	candidate := controlLifetimeLaunchRecord(l, facts, registrationDigest, timestamp())
	if err := validateControlLifetimeLaunch(l, candidate, registrationDigest); err != nil {
		return nil, err
	}
	created, putErr := tx.Put(controlLifetimeLaunchName(facts.Sequence), candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winnerLaunches, winnerErr := readControlLifetimeLaunchesTx(tx, l, registrationDigest)
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
func (l *ControlLifetime) ReadLifetimeLaunches() ([]task.LifetimeLaunchRecord, error) {
	return l.ReadLifetimeLaunchesContext(context.Background())
}

// ReadLifetimeLaunchesContext reads all contiguous launch records under the
// caller's bounded transaction lifetime.
func (l *ControlLifetime) ReadLifetimeLaunchesContext(ctx context.Context) (result []task.LifetimeLaunchRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = (controlLifetimeTransactionReader{tx: tx, lifetime: l}).ReadLifetimeLaunchesContext(ctx)
		return readErr
	})
	return result, err
}

// RecordLifetimeTermination persists the guardian's independently proven
// empty-scope observation and fences all later launches.
func (l *ControlLifetime) RecordLifetimeTermination(cause string) (*task.LifetimeTerminationRecord, error) {
	return l.RecordLifetimeTerminationContext(context.Background(), cause)
}

// RecordLifetimeTerminationContext persists termination under the caller's
// bounded transaction lifetime. ScopeEmpty is set only after the caller has
// independently proved the scope is empty.
func (l *ControlLifetime) RecordLifetimeTerminationContext(ctx context.Context, cause string) (result *task.LifetimeTerminationRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var transactionErr error
		result, transactionErr = recordControlLifetimeTerminationTx(tx, l, cause)
		return transactionErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeTerminationTx(tx *ControlTransaction, l *ControlLifetime, cause string) (*task.LifetimeTerminationRecord, error) {
	_, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
	if readErr != nil {
		return nil, readErr
	}
	launches, launchesErr := readControlLifetimeLaunchesTx(tx, l, registrationDigest)
	if launchesErr != nil {
		return nil, launchesErr
	}
	launchSetDigest, digestErr := lifetimeLaunchSetDigest(launches)
	if digestErr != nil {
		return nil, digestErr
	}
	candidate := task.LifetimeTerminationRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             l.rootID,
		TaskID:             l.operationID,
		RegistrationSHA256: registrationDigest,
		LaunchCount:        uint64(len(launches)),
		LaunchSetSHA256:    launchSetDigest,
		Cause:              cause,
		ScopeEmpty:         true,
		ObservedAt:         timestamp(),
	}
	if err := validateControlLifetimeTermination(l, candidate, registrationDigest, launches); err != nil {
		return nil, err
	}

	existing, terminationErr := readControlLifetimeTerminationTx(tx, l, registrationDigest, launches)
	if terminationErr == nil {
		candidate.ObservedAt = existing.ObservedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(terminationErr) {
		return nil, terminationErr
	}

	created, putErr := tx.Put(controlLifetimeTerminationName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeTerminationTx(tx, l, registrationDigest, launches)
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
func (l *ControlLifetime) ReadLifetimeTermination() (*task.LifetimeTerminationRecord, error) {
	return l.ReadLifetimeTerminationContext(context.Background())
}

// ReadLifetimeTerminationContext validates termination against the complete
// canonical launch set under the caller's bounded transaction lifetime.
func (l *ControlLifetime) ReadLifetimeTerminationContext(ctx context.Context) (result *task.LifetimeTerminationRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readBoundControlLifetimeTerminationTx(tx, l)
		return readErr
	})
	return result, err
}

func readBoundControlLifetimeTerminationTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeTerminationRecord, error) {
	var record task.LifetimeTerminationRecord
	if err := tx.Read(controlLifetimeTerminationName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeTerminationName, err)
	}
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return nil, err
	}
	_, digest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	launches, err := readControlLifetimeLaunchesTx(tx, l, digest)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	return readControlLifetimeTerminationTx(tx, l, digest, launches)
}

// HasTerminatedLifetime reports whether a strictly validated empty-scope
// termination exists, optionally for one cause.
func (l *ControlLifetime) HasTerminatedLifetime(cause string) (bool, error) {
	return l.HasTerminatedLifetimeContext(context.Background(), cause)
}

// HasTerminatedLifetimeContext reports termination under a bounded read.
func (l *ControlLifetime) HasTerminatedLifetimeContext(ctx context.Context, cause string) (bool, error) {
	record, err := l.ReadLifetimeTerminationContext(ctx)
	if task.RecordAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.ScopeEmpty && (cause == "" || record.Cause == cause), nil
}

func (l *ControlLifetime) validateLifetimeOperation(writing bool) error {
	if l == nil || l.control == nil {
		if writing {
			return task.ErrEvidenceFault
		}
		return os.ErrClosed
	}
	if err := task.ValidateRootID(l.rootID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(l.operationID); err != nil {
		return err
	}
	deadline, err := controlLifetimeDeadlineText(l.deadline)
	if err != nil {
		return err
	}
	if err := validateControlLifetimeOwner(l.rootID, l.operationID, l.ownerKind, l.ownerSHA256, deadline); err != nil {
		return err
	}
	return nil
}

func validateControlLifetimeOwner(rootID, operationID, ownerKind, ownerSHA256, deadline string) error {
	probe := task.LifetimeRegistrationRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		TaskID:        operationID,
		OwnerKind:     ownerKind,
		OwnerSHA256:   ownerSHA256,
		ScopeKind:     task.LifetimeScopeProcessGroup,
		Assurance:     task.LifetimeAssuranceCooperative,
		ScopeID:       "control-lifetime",
		BootID:        "control-lifetime",
		Deadline:      deadline,
		CreatedAt:     deadline,
	}
	return task.ValidateLifetimeRegistrationRecord(&probe)
}

func controlLifetimeDeadlineText(deadline time.Time) (string, error) {
	if deadline.IsZero() {
		return "", errors.New("missing control lifetime deadline")
	}
	return deadline.UTC().Format(time.RFC3339Nano), nil
}

func (l *ControlLifetime) deadlineText() string {
	return l.deadline.UTC().Format(time.RFC3339Nano)
}

func validateControlLifetimeRegistration(l *ControlLifetime, record task.LifetimeRegistrationRecord) error {
	if err := task.ValidateLifetimeRegistrationRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.OwnerKind != l.ownerKind || record.OwnerSHA256 != l.ownerSHA256 || record.Deadline != l.deadlineText() {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateControlLifetimeIntent(l *ControlLifetime, record task.LifetimeIntentRecord) error {
	if err := task.ValidateLifetimeIntentRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.OwnerKind != l.ownerKind || record.OwnerSHA256 != l.ownerSHA256 || record.Deadline != l.deadlineText() {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateControlLifetimeWorker(l *ControlLifetime, record task.LifetimeWorkerRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string) error {
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.RegistrationSHA256 != registrationDigest || registration == nil || record.BootID != registration.BootID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateControlLifetimeWorkerWait(l *ControlLifetime, record task.LifetimeWorkerWaitRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string, worker *task.LifetimeWorkerRecord, workerDigest string) error {
	if record.RootID != l.rootID || record.TaskID != l.operationID {
		return task.ErrIdentityMismatch
	}
	return validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest)
}

func validateControlLifetimeLaunch(l *ControlLifetime, record task.LifetimeLaunchRecord, registrationDigest string) error {
	if err := task.ValidateLifetimeLaunchRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.RegistrationSHA256 != registrationDigest || record.Sequence == 0 || record.Sequence > task.MaxLifetimeLaunches {
		return task.ErrIdentityMismatch
	}
	started, err := controlLifetimeCanonicalTimestamp("control lifetime launch", record.StartedAt)
	if err != nil {
		return err
	}
	deadline, err := controlLifetimeCanonicalTimestamp("control lifetime deadline", l.deadlineText())
	if err != nil || !started.Before(deadline) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateControlLifetimeTermination(l *ControlLifetime, record task.LifetimeTerminationRecord, registrationDigest string, launches []task.LifetimeLaunchRecord) error {
	if err := task.ValidateLifetimeTerminationRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.RegistrationSHA256 != registrationDigest || record.LaunchCount != uint64(len(launches)) {
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

func readControlLifetimeRegistrationTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeRegistrationRecord, string, error) {
	var record task.LifetimeRegistrationRecord
	if err := tx.Read(controlLifetimeRegistrationName, &record); err != nil {
		return nil, "", controlLifetimeReadError(controlLifetimeRegistrationName, err)
	}
	if err := validateControlLifetimeRegistration(l, record); err != nil {
		return nil, "", err
	}
	if err := tx.Verify(controlLifetimeRegistrationName, record); err != nil {
		return nil, "", err
	}
	digest, err := controlLifetimeRecordDigest(record)
	if err != nil {
		return nil, "", err
	}
	return &record, digest, nil
}

func readControlLifetimeIntentTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeIntentRecord, error) {
	var record task.LifetimeIntentRecord
	if err := tx.Read(controlLifetimeIntentName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeIntentName, err)
	}
	if err := validateControlLifetimeIntent(l, record); err != nil {
		return nil, err
	}
	if err := tx.Verify(controlLifetimeIntentName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readControlLifetimeWorkerTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerRecord, error) {
	var record task.LifetimeWorkerRecord
	if err := tx.Read(controlLifetimeWorkerName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeWorkerName, err)
	}
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	registration, registrationDigest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err = validateControlLifetimeWorker(l, record, registration, registrationDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(controlLifetimeWorkerName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readControlLifetimeWorkerBindingTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerRecord, string, error) {
	worker, err := readControlLifetimeWorkerTx(tx, l)
	if task.RecordAbsent(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	digest, err := controlLifetimeRecordDigest(worker)
	if err != nil {
		return nil, "", err
	}
	return worker, digest, nil
}

func readControlLifetimeWorkerWaitTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerWaitRecord, error) {
	var record task.LifetimeWorkerWaitRecord
	if err := tx.Read(controlLifetimeWorkerWaitName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeWorkerWaitName, err)
	}
	if err := task.ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		return nil, err
	}
	if record.Result == task.LifetimeWorkerWaitAbsent {
		evidence, err := loadLifetimeDependenciesContext(tx.ctx, controlLifetimeTransactionReader{tx: tx, lifetime: l})
		if err != nil {
			return nil, lifetimeCompletionDependencyError(err)
		}
		if record.RootID != l.rootID || record.TaskID != l.operationID {
			return nil, task.ErrIdentityMismatch
		}
		if err = task.ValidateLifetimeCompletionBinding(&record, evidence); err != nil {
			return nil, err
		}
		if err = tx.Verify(controlLifetimeWorkerWaitName, record); err != nil {
			return nil, err
		}
		return &record, nil
	}
	registration, registrationDigest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	worker, workerDigest, err := readControlLifetimeWorkerBindingTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err = validateControlLifetimeWorkerWait(l, record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(controlLifetimeWorkerWaitName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readControlLifetimeLaunchesTx(tx *ControlTransaction, l *ControlLifetime, registrationDigest string) ([]task.LifetimeLaunchRecord, error) {
	launches := make([]task.LifetimeLaunchRecord, 0, task.MaxLifetimeLaunches)
	missing := false
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches+1; sequence++ {
		name := controlLifetimeLaunchName(sequence)
		var record task.LifetimeLaunchRecord
		err := tx.Read(name, &record)
		if errors.Is(err, os.ErrNotExist) {
			if sequence <= task.MaxLifetimeLaunches {
				missing = true
			}
			continue
		}
		if err != nil {
			return nil, controlLifetimeReadError(name, err)
		}
		if sequence > task.MaxLifetimeLaunches {
			return nil, fmt.Errorf("%w: more than %d lifetime launches", task.ErrEvidenceFault, task.MaxLifetimeLaunches)
		}
		if missing {
			return nil, task.ErrIdentityMismatch
		}
		if err := validateControlLifetimeLaunch(l, record, registrationDigest); err != nil {
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

func readControlLifetimeTerminationTx(tx *ControlTransaction, l *ControlLifetime, registrationDigest string, launches []task.LifetimeLaunchRecord) (*task.LifetimeTerminationRecord, error) {
	var record task.LifetimeTerminationRecord
	if err := tx.Read(controlLifetimeTerminationName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeTerminationName, err)
	}
	if err := validateControlLifetimeTermination(l, record, registrationDigest, launches); err != nil {
		return nil, err
	}
	if err := tx.Verify(controlLifetimeTerminationName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func controlLifetimeLaunchRecord(l *ControlLifetime, facts LifetimeLaunchFacts, registrationDigest, startedAt string) task.LifetimeLaunchRecord {
	return task.LifetimeLaunchRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             l.rootID,
		TaskID:             l.operationID,
		RegistrationSHA256: registrationDigest,
		Sequence:           facts.Sequence,
		PID:                facts.PID,
		ProcessStart:       facts.ProcessStart,
		PGID:               facts.PGID,
		StartedAt:          startedAt,
	}
}

type controlLifetimeDigestRecord interface {
	task.LifetimeRegistrationRecord | *task.LifetimeRegistrationRecord |
		task.LifetimeWorkerRecord | *task.LifetimeWorkerRecord
}

func controlLifetimeRecordDigest[T controlLifetimeDigestRecord](record T) (string, error) {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func controlLifetimeReadError(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: invalid persisted %s: %w", task.ErrEvidenceFault, name, err)
}

func controlLifetimeCanonicalTimestamp(name, value string) (time.Time, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %w", name, err)
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, fmt.Errorf("%s is not canonical UTC RFC3339Nano", name)
	}
	return parsed, nil
}

func withControlLifetimeTransactionContext(ctx context.Context, control *ControlDir, fn func(*ControlTransaction) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return control.WithLock(ctx, fn)
}

func controlLifetimeLaunchName(sequence uint64) string {
	return fmt.Sprintf("%s%020d%s", controlLifetimeLaunchPrefix, sequence, controlLifetimeLaunchSuffix)
}
