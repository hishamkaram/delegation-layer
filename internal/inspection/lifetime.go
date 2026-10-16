package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"

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

func (o *Operation) controlLifetime(writing bool) (*taskdir.ControlLifetime, error) {
	if err := o.validateLifetimeOperation(writing); err != nil {
		return nil, err
	}
	return taskdir.NewControlLifetime(o.control, o.request.RootID, o.request.IdentityID(), task.LifetimeOwnerInspection, o.digest, o.deadline)
}

// AcquireLifetimeOwner retains the control directory's process-scope lease
// for the guardian lifetime. The guardian must release it after termination
// evidence is durable.
func (o *Operation) AcquireLifetimeOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	if o == nil || o.control == nil {
		return nil, task.ErrInvalidPermit
	}
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.AcquireLifetimeOwner(ctx)
}

// TryAcquireLifetimeOwner keeps observation from waiting on a live guardian.
func (o *Operation) TryAcquireLifetimeOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	if o == nil || o.control == nil {
		return nil, task.ErrInvalidPermit
	}
	if _, err := o.controlLifetime(false); err != nil {
		return nil, err
	}
	return o.control.TryAcquireLifetimeOwnerContext(ctx)
}

// RecordLifetimeIntent persists the inspection-owned scope plan before native
// scope creation.
func (o *Operation) RecordLifetimeIntent(facts taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	return o.RecordLifetimeIntentContext(context.Background(), facts)
}

// RecordLifetimeIntentContext persists the scope plan under the caller's
// bounded control transaction.
func (o *Operation) RecordLifetimeIntentContext(ctx context.Context, facts taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeIntentContext(ctx, facts)
}

// ReadLifetimeIntent returns the strictly validated inspection scope plan.
func (o *Operation) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	return o.ReadLifetimeIntentContext(context.Background())
}

// ReadLifetimeIntentContext reads the scope plan under a bounded transaction.
func (o *Operation) ReadLifetimeIntentContext(ctx context.Context) (*task.LifetimeIntentRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeIntentContext(ctx)
}

// RecordLifetimeRegistration persists the inspection-owned scope binding.
func (o *Operation) RecordLifetimeRegistration(facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	return o.RecordLifetimeRegistrationContext(context.Background(), facts)
}

// RecordLifetimeRegistrationContext persists the inspection-owned scope
// binding while honoring the caller's bounded transaction lifetime.
func (o *Operation) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeRegistrationContext(ctx, facts)
}

// ReadLifetimeRegistration returns the strictly validated inspection scope
// registration, including its canonical winner bytes.
func (o *Operation) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	return o.ReadLifetimeRegistrationContext(context.Background())
}

// ReadLifetimeRegistrationContext reads the registration under a bounded
// transaction lifetime.
func (o *Operation) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeRegistrationContext(ctx)
}

// LifetimeRegistrationDigest returns the digest of the canonical persisted
// registration record.
func (o *Operation) LifetimeRegistrationDigest() (string, error) {
	return o.LifetimeRegistrationDigestContext(context.Background())
}

// LifetimeRegistrationDigestContext returns the registration digest under a
// bounded transaction lifetime.
func (o *Operation) LifetimeRegistrationDigestContext(ctx context.Context) (string, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return "", err
	}
	return lifetime.LifetimeRegistrationDigestContext(ctx)
}

// RecordLifetimeWorker persists the managed worker identity for the current
// canonical registration.
func (o *Operation) RecordLifetimeWorker(facts taskdir.LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	return o.RecordLifetimeWorkerContext(context.Background(), facts)
}

// RecordLifetimeWorkerContext persists the worker identity under a bounded
// control transaction.
func (o *Operation) RecordLifetimeWorkerContext(ctx context.Context, facts taskdir.LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeWorkerContext(ctx, facts)
}

// ReadLifetimeWorker returns the worker identity bound to the current
// registration.
func (o *Operation) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	return o.ReadLifetimeWorkerContext(context.Background())
}

// ReadLifetimeWorkerContext reads the worker identity under a bounded
// transaction.
func (o *Operation) ReadLifetimeWorkerContext(ctx context.Context) (*task.LifetimeWorkerRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeWorkerContext(ctx)
}

// RecordLifetimeWorkerWait persists the create-once worker completion
// disposition under the inspection operation's control transaction.
func (o *Operation) RecordLifetimeWorkerWait(facts taskdir.LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	return o.RecordLifetimeWorkerWaitContext(context.Background(), facts)
}

// RecordLifetimeWorkerWaitContext persists a worker completion disposition
// while honoring the caller's bounded operation lifetime.
func (o *Operation) RecordLifetimeWorkerWaitContext(ctx context.Context, facts taskdir.LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeWorkerWaitContext(ctx, facts)
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (o *Operation) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	return o.ReadLifetimeWorkerWaitContext(context.Background())
}

// ReadLifetimeWorkerWaitContext reads the child Wait winner under a bounded
// control transaction.
func (o *Operation) ReadLifetimeWorkerWaitContext(ctx context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeWorkerWaitContext(ctx)
}

// RecordLifetimeLaunch persists one create-once native launch bound to the
// current registration. New sequences must extend the contiguous set.
func (o *Operation) RecordLifetimeLaunch(facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	return o.RecordLifetimeLaunchContext(context.Background(), facts)
}

// RecordLifetimeLaunchContext persists one native launch under the caller's
// bounded transaction lifetime.
func (o *Operation) RecordLifetimeLaunchContext(ctx context.Context, facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeLaunchContext(ctx, facts)
}

// ReadLifetimeLaunches probes the bounded canonical sequence namespace and
// rejects gaps, malformed winners, conflicting identities, and overflow.
func (o *Operation) ReadLifetimeLaunches() ([]task.LifetimeLaunchRecord, error) {
	return o.ReadLifetimeLaunchesContext(context.Background())
}

// ReadLifetimeLaunchesContext reads all contiguous launch records under the
// caller's bounded transaction lifetime.
func (o *Operation) ReadLifetimeLaunchesContext(ctx context.Context) ([]task.LifetimeLaunchRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeLaunchesContext(ctx)
}

// RecordLifetimeTermination persists the guardian's independently proven empty
// scope observation and fences all later launches.
func (o *Operation) RecordLifetimeTermination(cause string) (*task.LifetimeTerminationRecord, error) {
	return o.RecordLifetimeTerminationContext(context.Background(), cause)
}

// RecordLifetimeTerminationContext persists termination under the caller's
// bounded transaction lifetime.
func (o *Operation) RecordLifetimeTerminationContext(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
	lifetime, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return lifetime.RecordLifetimeTerminationContext(ctx, cause)
}

// ReadLifetimeTermination returns the strict create-once termination winner.
func (o *Operation) ReadLifetimeTermination() (*task.LifetimeTerminationRecord, error) {
	return o.ReadLifetimeTerminationContext(context.Background())
}

// ReadLifetimeTerminationContext validates termination against the complete
// canonical launch set under a bounded control transaction.
func (o *Operation) ReadLifetimeTerminationContext(ctx context.Context) (*task.LifetimeTerminationRecord, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return lifetime.ReadLifetimeTerminationContext(ctx)
}

// HasTerminatedLifetime reports whether a strictly validated empty-scope
// termination exists, optionally for one cause.
func (o *Operation) HasTerminatedLifetime(cause string) (bool, error) {
	return o.HasTerminatedLifetimeContext(context.Background(), cause)
}

// HasTerminatedLifetimeContext reports termination under a bounded read.
func (o *Operation) HasTerminatedLifetimeContext(ctx context.Context, cause string) (bool, error) {
	lifetime, err := o.controlLifetime(false)
	if err != nil {
		return false, err
	}
	return lifetime.HasTerminatedLifetimeContext(ctx, cause)
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
	if err := task.ValidateTaskID(o.request.IdentityID()); err != nil {
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

type lifetimeDigestRecord interface {
	task.LifetimeRegistrationRecord | *task.LifetimeRegistrationRecord |
		task.LifetimeWorkerRecord | *task.LifetimeWorkerRecord
}

func lifetimeRecordDigest[T lifetimeDigestRecord](record T) (string, error) {
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

func lifetimeDependencyError(err error) error {
	if err == nil ||
		errors.Is(err, task.ErrIdentityMismatch) ||
		errors.Is(err, task.ErrEvidenceFault) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(task.ErrIdentityMismatch, err)
}

func lifetimeLaunchSetDigest(launches []task.LifetimeLaunchRecord) (string, error) {
	data, err := task.MarshalCanonical(launches)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func inspectionLifetimeLaunchName(sequence uint64) string {
	return fmt.Sprintf("%s%020d%s", inspectionLifetimeLaunchPrefix, sequence, inspectionLifetimeLaunchSuffix)
}

// RecordLifetimeWorkerAbsenceContext preserves a recovery disposition without
// turning worker absence into successful inspection evidence.
func (o *Operation) RecordLifetimeWorkerAbsenceContext(ctx context.Context, proof task.LifetimeAbsenceProof, observedBootID string) (*task.LifetimeWorkerWaitRecord, error) {
	records, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return records.RecordLifetimeWorkerAbsenceContext(ctx, proof, observedBootID)
}

// ReadLifetimeUnlaunchedContext reads the existing no-native-start fence.
func (o *Operation) ReadLifetimeUnlaunchedContext(ctx context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	records, err := o.controlLifetime(false)
	if err != nil {
		return nil, err
	}
	return records.ReadLifetimeUnlaunchedContext(ctx)
}

// SealLifetimeUnlaunchedContext applies only to immutable guardian-owned
// inspections. Historical/custom workers do not promise native intent first.
func (o *Operation) SealLifetimeUnlaunchedContext(ctx context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	if o == nil || o.request.Binding.WorkerExitProof != WorkerExitProofGuardianWaitV1 {
		return nil, task.ErrInvalidPermit
	}
	records, err := o.controlLifetime(true)
	if err != nil {
		return nil, err
	}
	return records.SealLifetimeUnlaunchedContext(ctx)
}

func (o *Operation) withLifetimeLaunchPermissionContext(ctx context.Context, callback func(*taskdir.ControlTransaction) error) error {
	records, err := o.controlLifetime(true)
	if err != nil {
		return err
	}
	return records.WithLifetimeLaunchPermissionContext(ctx, callback)
}
