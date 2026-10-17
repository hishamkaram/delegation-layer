package guardian

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	defaultLifetimeGuardianCleanupTimeout = 5 * time.Second
	defaultLifetimeGuardianStopPoll       = time.Second
	defaultLifetimeWorkerWaitRetry        = 100 * time.Millisecond
	defaultLifetimeFinalizationBackoff    = 100 * time.Millisecond
	maxLifetimeFinalizationBackoff        = 5 * time.Second
	lifetimeTerminationProofAttempts      = 3
	lifetimeScopeReleaseAttempts          = 3
	lifetimeGuardianCodeInvalidOrder      = "invalid-order"
	lifetimeGuardianCodeDeadline          = "deadline"
	lifetimeGuardianCodeStarted           = "started-failed"
	lifetimeGuardianCodeFinish            = "finish-failed"
	lifetimeGuardianCodeTermination       = "termination-failed"
)

var (
	errLifetimeGuardianInvalid       = errors.New("task lifetime guardian: invalid configuration")
	errLifetimeGuardianOrder         = errors.New("task lifetime guardian: invalid message order")
	errLifetimeGuardianScopeNotEmpty = errors.New("task lifetime guardian: scope is not independently empty")
	errLifetimeGuardianLaunchRecord  = errors.New("task lifetime guardian: launch record is not durable")
	errLifetimeGuardianWorkerLost    = errors.New("task lifetime guardian: worker lost")
	errLifetimeGuardianDeadline      = errors.New("task lifetime guardian: deadline reached")
	errLifetimeGuardianStopRequested = errors.New("task lifetime guardian: stop requested")
)

// Owner is the consumer-owned part of taskdir's lifetime
// lease. The lease is acquired before the scope is created and retained until
// the guardian has finished all scope and record operations.
type Owner interface {
	Release() error
}

// Records contains only the durable lifetime record operations
// needed by the state machine. Guardian owns their ordering policy.
type Records interface {
	ReadLifetimeWorkerWaitContext(context.Context) (*task.LifetimeWorkerWaitRecord, error)
	ReadLifetimeTerminationContext(context.Context) (*task.LifetimeTerminationRecord, error)
	ReadLifetimeIntentContext(context.Context) (*task.LifetimeIntentRecord, error)
	ReadLifetimeRegistrationContext(context.Context) (*task.LifetimeRegistrationRecord, error)
	RecordLifetimeIntentContext(context.Context, taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error)
	RecordLifetimeRegistrationContext(context.Context, taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error)
	RecordLifetimeWorkerContext(context.Context, taskdir.LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error)
	RecordLifetimeWorkerWaitContext(context.Context, taskdir.LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error)
	RecordLifetimeLaunchContext(context.Context, taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error)
	RecordLifetimeTerminationContext(context.Context, string) (*task.LifetimeTerminationRecord, error)
}

// lifetimeUnlaunchedReader is optional so existing task and inspection test
// records retain their narrow lifetime contract. Workspace guardians must
// provide it because their launch fence is an additional terminal refusal.
type lifetimeTaskLaunchRefusalReader interface {
	TaskLaunchRefusedContext(context.Context) (bool, error)
}

type lifetimeUnlaunchedReader interface {
	ReadLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error)
}

// Scope is deliberately narrower than lifetime.Scope's
// implementation. The guardian observes and terminates a scope; the worker
// configures its own provider commands through the existing wire client.
type Scope interface {
	Reference() lifetime.Ref
	BindOwnerProcess() error
	TrackOwnerProcess(lifetime.ProcessRef) error
	ObserveStarted(int) (lifetime.LaunchRef, error)
	Terminate(context.Context, []lifetime.LaunchRef) error
	EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error)
	PendingStartEmptyContext(context.Context) (bool, error)
	Release() error
	DestroyEmpty() error
}

// Options supplies identity, storage, and native boundaries for one guardian.
// New copies this configuration; runtime transition state belongs to Guardian.
// Nil primitive and clock hooks select the production implementations.
type Options struct {
	RootID    string
	TaskID    string
	OwnerKind string
	Deadline  time.Time

	Records                Records
	AcquireOwner           func(context.Context) (Owner, error)
	ReconcileOwned         func(context.Context) error
	Worker                 *exec.Cmd
	Connection             io.ReadWriteCloser
	PlanFactory            func(string, string, string) (lifetime.Plan, error)
	ScopeFactory           func(lifetime.Plan) (Scope, error)
	BootID                 func() (string, error)
	ObserveWorker          func(*exec.Cmd) (lifetime.ProcessRef, error)
	StartWorker            func(*exec.Cmd) error
	WaitWorker             func(*exec.Cmd) error
	WaitRecordRetry        func(context.Context) error
	TerminationRecordRetry func(context.Context) error
	FinalizationBackoff    func(context.Context, time.Duration) error
	CleanupTimeout         time.Duration
	Now                    func() time.Time
	UserStop               func() (bool, error)
	StopPoll               time.Duration
}

// New constructs the canonical lifecycle owner. The option
// seam keeps the state machine deterministic without replacing taskdir or
// lifetime owners.
func New(options Options) (*Guardian, error) {
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = defaultLifetimeGuardianCleanupTimeout
	}
	if options.UserStop != nil && options.StopPoll == 0 {
		options.StopPoll = defaultLifetimeGuardianStopPoll
	}
	if options.OwnerKind == "" {
		options.OwnerKind = task.LifetimeOwnerTask
	}
	if options.PlanFactory == nil {
		options.PlanFactory = lifetime.Prepare
	}
	if options.ScopeFactory == nil {
		options.ScopeFactory = func(plan lifetime.Plan) (Scope, error) {
			return lifetime.CreatePrepared(plan)
		}
	}
	if options.BootID == nil {
		options.BootID = func() (string, error) {
			return lifetime.BootID()
		}
	}
	if options.ObserveWorker == nil {
		options.ObserveWorker = func(cmd *exec.Cmd) (lifetime.ProcessRef, error) {
			if cmd == nil || cmd.Process == nil {
				return lifetime.ProcessRef{}, errLifetimeGuardianInvalid
			}
			return lifetime.ObserveStartedProcess(cmd.Process.Pid)
		}
	}
	if options.StartWorker == nil {
		options.StartWorker = func(cmd *exec.Cmd) error {
			return cmd.Start()
		}
	}
	if options.WaitWorker == nil {
		options.WaitWorker = func(cmd *exec.Cmd) error {
			return cmd.Wait()
		}
	}
	options.WaitRecordRetry = resolveLifetimeWorkerRecordRetry(options.WaitRecordRetry)
	options.TerminationRecordRetry = resolveLifetimeWorkerRecordRetry(options.TerminationRecordRetry)
	options.FinalizationBackoff = resolveLifetimeFinalizationBackoff(options.FinalizationBackoff)
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := validateLifetimeGuardianOptions(options); err != nil {
		return nil, err
	}
	return &Guardian{options: options}, nil
}

func validateLifetimeGuardianOptions(options Options) error {
	validators := [...]func(Options) error{
		validateLifetimeGuardianIdentity,
		validateLifetimeGuardianProofs,
		validateLifetimeGuardianRuntime,
		validateLifetimeGuardianTiming,
	}
	for _, validate := range validators {
		if err := validate(options); err != nil {
			return err
		}
	}
	return nil
}

func validateLifetimeGuardianIdentity(options Options) error {
	if task.ValidateRootID(options.RootID) != nil || task.ValidateTaskID(options.TaskID) != nil {
		return fmt.Errorf("%w: root and task IDs are required", errLifetimeGuardianInvalid)
	}
	if options.OwnerKind != task.LifetimeOwnerTask && options.OwnerKind != task.LifetimeOwnerInspection && options.OwnerKind != task.LifetimeOwnerWorkspace {
		return fmt.Errorf("%w: lifetime owner kind is invalid", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianProofs(options Options) error {
	if options.OwnerKind != task.LifetimeOwnerTask && options.UserStop != nil {
		return fmt.Errorf("%w: user stop polling requires a task owner", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianRuntime(options Options) error {
	if options.Deadline.IsZero() {
		return fmt.Errorf("%w: deadline is required", errLifetimeGuardianInvalid)
	}
	if options.Records == nil || options.AcquireOwner == nil || options.ReconcileOwned == nil || options.Worker == nil || options.Connection == nil {
		return fmt.Errorf("%w: owner, records, worker, and control connection are required", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianTiming(options Options) error {
	if options.CleanupTimeout < 0 {
		return fmt.Errorf("%w: cleanup timeout must be positive", errLifetimeGuardianInvalid)
	}
	if options.StopPoll < 0 || (options.UserStop != nil && options.StopPoll == 0) || (options.UserStop == nil && options.StopPoll != 0) {
		return fmt.Errorf("%w: user stop polling is invalid", errLifetimeGuardianInvalid)
	}
	return nil
}
