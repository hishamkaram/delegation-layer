package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
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

// lifetimeGuardianOwner is the consumer-owned part of taskdir's lifetime
// lease. The lease is acquired before the scope is created and retained until
// the guardian has finished all scope and record operations.
type lifetimeGuardianOwner interface {
	Release() error
}

// lifetimeGuardianRecords contains only the durable lifetime record operations
// needed by the state machine. The application owns their ordering policy.
type lifetimeGuardianRecords interface {
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

// lifetimeGuardianScope is deliberately narrower than lifetime.Scope's
// implementation. The guardian observes and terminates a scope; the worker
// configures its own provider commands through the existing wire client.
type lifetimeGuardianScope interface {
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

type lifetimeGuardianOptions struct {
	rootID    string
	taskID    string
	ownerKind string
	deadline  time.Time

	records                lifetimeGuardianRecords
	acquireOwner           func(context.Context) (lifetimeGuardianOwner, error)
	reconcileOwned         func(context.Context) error
	worker                 *exec.Cmd
	connection             io.ReadWriteCloser
	planFactory            func(string, string, string) (lifetime.Plan, error)
	scopeFactory           func(lifetime.Plan) (lifetimeGuardianScope, error)
	bootID                 func() (string, error)
	observeWorker          func(*exec.Cmd) (lifetime.ProcessRef, error)
	startWorker            func(*exec.Cmd) error
	waitWorker             func(*exec.Cmd) error
	waitRecordRetry        func(context.Context) error
	terminationRecordRetry func(context.Context) error
	finalizationBackoff    func(context.Context, time.Duration) error
	cleanupTimeout         time.Duration
	now                    func() time.Time
	userStop               func() (bool, error)
	stopPoll               time.Duration
}

type lifetimeGuardian struct {
	options lifetimeGuardianOptions

	runMu   sync.Mutex
	running bool

	owner  lifetimeGuardianOwner
	scope  lifetimeGuardianScope
	codec  *lifetimeCodec
	bootID string

	registrationRecorded   bool
	unregisteredDisposed   bool
	terminationRecorded    bool
	terminationEmptyProven bool
	terminationCause       string
	helloReceived          bool
	fenced                 bool
	terminal               bool
	workerStarted          bool
	workerPID              int
	workerSHA256           string
	workerWait             <-chan error
	workerWaitObserved     bool
	workerWaitErr          error
	workerWaitFacts        *taskdir.LifetimeWorkerWaitFacts
	workerWaitRecorded     bool
	pendingStartFenced     bool
	scopeDestroyed         bool
	scopeReleased          bool
	ownerReleased          bool
	authorizedSequence     uint64
	activeSequence         uint64
	nextSequence           uint64
	launchRefs             []lifetime.LaunchRef
}

type lifetimeGuardianReadResult struct {
	message lifetimeMessage
	err     error
}

type lifetimeGuardianReader struct {
	events chan lifetimeGuardianReadResult
	done   chan struct{}
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

type lifetimeGuardianServeResult struct {
	err             error
	continueServing bool
}

// newLifetimeGuardian constructs the app-owned lifecycle policy. The option
// seam keeps the state machine deterministic without replacing taskdir or
// lifetime owners.
func newLifetimeGuardian(options lifetimeGuardianOptions) (*lifetimeGuardian, error) {
	if options.cleanupTimeout == 0 {
		options.cleanupTimeout = defaultLifetimeGuardianCleanupTimeout
	}
	if options.userStop != nil && options.stopPoll == 0 {
		options.stopPoll = defaultLifetimeGuardianStopPoll
	}
	if options.ownerKind == "" {
		options.ownerKind = task.LifetimeOwnerTask
	}
	if options.planFactory == nil {
		options.planFactory = lifetime.Prepare
	}
	if options.scopeFactory == nil {
		options.scopeFactory = func(plan lifetime.Plan) (lifetimeGuardianScope, error) {
			return lifetime.CreatePrepared(plan)
		}
	}
	if options.bootID == nil {
		options.bootID = func() (string, error) {
			return lifetime.BootID()
		}
	}
	if options.observeWorker == nil {
		options.observeWorker = func(cmd *exec.Cmd) (lifetime.ProcessRef, error) {
			if cmd == nil || cmd.Process == nil {
				return lifetime.ProcessRef{}, errLifetimeGuardianInvalid
			}
			return lifetime.ObserveStartedProcess(cmd.Process.Pid)
		}
	}
	if options.startWorker == nil {
		options.startWorker = func(cmd *exec.Cmd) error {
			return cmd.Start()
		}
	}
	if options.waitWorker == nil {
		options.waitWorker = func(cmd *exec.Cmd) error {
			return cmd.Wait()
		}
	}
	options.waitRecordRetry = resolveLifetimeWorkerRecordRetry(options.waitRecordRetry)
	options.terminationRecordRetry = resolveLifetimeWorkerRecordRetry(options.terminationRecordRetry)
	options.finalizationBackoff = resolveLifetimeFinalizationBackoff(options.finalizationBackoff)
	if options.now == nil {
		options.now = time.Now
	}
	if err := validateLifetimeGuardianOptions(options); err != nil {
		return nil, err
	}
	return &lifetimeGuardian{options: options}, nil
}

func validateLifetimeGuardianOptions(options lifetimeGuardianOptions) error {
	validators := [...]func(lifetimeGuardianOptions) error{
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

func validateLifetimeGuardianIdentity(options lifetimeGuardianOptions) error {
	if task.ValidateRootID(options.rootID) != nil || task.ValidateTaskID(options.taskID) != nil {
		return fmt.Errorf("%w: root and task IDs are required", errLifetimeGuardianInvalid)
	}
	if options.ownerKind != task.LifetimeOwnerTask && options.ownerKind != task.LifetimeOwnerInspection && options.ownerKind != task.LifetimeOwnerWorkspace {
		return fmt.Errorf("%w: lifetime owner kind is invalid", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianProofs(options lifetimeGuardianOptions) error {
	if options.ownerKind != task.LifetimeOwnerTask && options.userStop != nil {
		return fmt.Errorf("%w: user stop polling requires a task owner", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianRuntime(options lifetimeGuardianOptions) error {
	if options.deadline.IsZero() {
		return fmt.Errorf("%w: deadline is required", errLifetimeGuardianInvalid)
	}
	if options.records == nil || options.acquireOwner == nil || options.reconcileOwned == nil || options.worker == nil || options.connection == nil {
		return fmt.Errorf("%w: owner, records, worker, and control connection are required", errLifetimeGuardianInvalid)
	}
	return nil
}

func validateLifetimeGuardianTiming(options lifetimeGuardianOptions) error {
	if options.cleanupTimeout < 0 {
		return fmt.Errorf("%w: cleanup timeout must be positive", errLifetimeGuardianInvalid)
	}
	if options.stopPoll < 0 || (options.userStop != nil && options.stopPoll == 0) || (options.userStop == nil && options.stopPoll != 0) {
		return fmt.Errorf("%w: user stop polling is invalid", errLifetimeGuardianInvalid)
	}
	return nil
}

func taskUserStopRequested(td *taskdir.TaskDir) (bool, error) {
	return td.UserStopRequested()
}

// Run acquires the task lifetime owner, registers the strongest available
// scope, starts one worker, and then serves the private lifetime protocol.
func (g *lifetimeGuardian) Run(ctx context.Context) (resultErr error) {
	if g == nil || ctx == nil {
		return errLifetimeGuardianInvalid
	}
	if !g.beginRun() {
		return fmt.Errorf("%w: guardian already running", errLifetimeGuardianInvalid)
	}
	defer g.endRun()
	return g.runOwned(ctx)
}

// runOwned holds the lifetime-owner lease across scope creation, worker
// execution, and finalization. Native termination becomes durable before the
// guardian waits for the worker, because that worker may itself be waiting for
// a provider process owned by the scope.
func (g *lifetimeGuardian) runOwned(ctx context.Context) (resultErr error) {
	_, err := g.acquireRunOwner(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, g.finalizeOwnedRun(ctx)) }()
	if err = g.refuseTerminalReplay(ctx); err != nil {
		return err
	}
	if g.deadlineReached() {
		return g.recoverExpiredStart(ctx)
	}

	bootID, scope, err := g.prepareRunScope(ctx)
	if err != nil {
		return err
	}
	g.scope = scope
	g.bootID = bootID
	if err = g.bindAndRecordRunScope(ctx, bootID); err != nil {
		return err
	}
	if g.deadlineReached() {
		return g.recoverWithCauseAndKind(ctx, errLifetimeGuardianDeadline, task.LifetimeTerminationBudget)
	}
	codec, err := newLifetimeCodec(g.options.connection)
	if err != nil {
		cause := fmt.Errorf("create lifetime control codec: %w", err)
		return errors.Join(cause, g.recoverWithCause(ctx, cause))
	}
	g.codec = codec
	receiveCtx, cancelReceive := context.WithDeadline(ctx, g.options.deadline)
	defer cancelReceive()
	reader := startLifetimeGuardianReader(codec, receiveCtx)

	waitDone, err := g.startAndRecordWorker(ctx, reader, bootID)
	if err != nil {
		return err
	}

	serveResult := g.serve(ctx, reader, waitDone)
	readerErr := g.stopReader(ctx, reader)
	return errors.Join(serveResult.err, readerErr)
}

// recoverExpiredStart refuses fresh work after its immutable deadline while
// preserving recovery authority for durable state left by an earlier owner.
func (g *lifetimeGuardian) recoverExpiredStart(ctx context.Context) error {
	intent, err := optionalLifetimeIntent(g.options.records.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return errors.Join(errLifetimeGuardianDeadline, err)
	}
	registration, err := optionalLifetimeRegistration(g.options.records.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return errors.Join(errLifetimeGuardianDeadline, err)
	}
	if intent == nil && registration == nil {
		return errLifetimeGuardianDeadline
	}
	recoveryCtx, cancel := g.recoveryContext(ctx)
	defer cancel()
	return errors.Join(errLifetimeGuardianDeadline, g.options.reconcileOwned(recoveryCtx))
}

// finalizeOwnedRun is the only release boundary for a live guardian. A
// registered scope and its owner lease remain held until both terminal scope
// evidence and the worker completion disposition are durable. Production
// retries use an uncancelled context because no other process can reconstruct
// these handles.
func (g *lifetimeGuardian) finalizeOwnedRun(parent context.Context) error {
	if g.owner == nil || g.ownerReleased {
		return nil
	}
	if parent == nil {
		return errLifetimeGuardianInvalid
	}
	finalizationCtx := context.WithoutCancel(parent)
	if g.scope == nil {
		return g.releaseOwnerUntilDone(finalizationCtx)
	}
	finalized, err := g.finalizeScopeRegistration(finalizationCtx)
	if err != nil || finalized {
		return err
	}
	waitErr, err := g.persistFinalLifetimeEvidence(finalizationCtx)
	if err != nil {
		return errors.Join(waitErr, err)
	}
	return g.releaseFinalizedScope(finalizationCtx, waitErr)
}

// finalizeScopeRegistration resolves the only state that may precede a
// durable registration. It reports finalized after an unregistered scope was
// safely disposed and both owners were released.
func (g *lifetimeGuardian) finalizeScopeRegistration(ctx context.Context) (bool, error) {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if g.unregisteredDisposed {
			if err := g.releaseScopeUntilDone(ctx); err != nil {
				return false, err
			}
			return true, g.releaseOwnerUntilDone(ctx)
		}
		if g.registrationRecorded {
			return false, nil
		}
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := g.resolveCreatedScopeFailure(attemptCtx, g.bootID)
		cancel()
		if err == nil {
			backoff = defaultLifetimeFinalizationBackoff
			continue
		}
		if g.registrationRecorded || g.unregisteredDisposed {
			continue
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.terminationRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *lifetimeGuardian) persistFinalLifetimeEvidence(ctx context.Context) (waitErr, recordErr error) {
	if g.terminationCause == "" {
		g.terminationCause = task.LifetimeTerminationRecovery
	}
	if err := g.persistTermination(ctx); err != nil {
		return nil, err
	}
	if g.workerStarted {
		waitErr = g.awaitWorkerWaitUntilDone(ctx)
	}
	if !g.workerWaitRecorded {
		if err := g.persistWorkerWait(ctx); err != nil {
			return waitErr, err
		}
	}
	return waitErr, nil
}

func (g *lifetimeGuardian) awaitWorkerWaitUntilDone(ctx context.Context) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		observed, err := g.awaitWorkerWait(ctx)
		if observed {
			return err
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *lifetimeGuardian) releaseFinalizedScope(ctx context.Context, waitErr error) error {
	if err := g.destroyScopeUntilDone(ctx); err != nil {
		return errors.Join(waitErr, err)
	}
	if err := g.releaseScopeUntilDone(ctx); err != nil {
		return errors.Join(waitErr, err)
	}
	return errors.Join(waitErr, g.releaseOwnerUntilDone(ctx))
}

// destroyScopeUntilDone, releaseScopeUntilDone, and releaseOwnerUntilDone are
// deliberately resident. Once registration is durable, returning from the
// guardian would leave a native scope whose owner can no longer reconstruct
// the exact local handles. Each helper retries only its current phase so a
// completed transition is never repeated.
func (g *lifetimeGuardian) destroyScopeUntilDone(ctx context.Context) error {
	if g.scope == nil || g.scopeDestroyed {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.scopeDestroyed {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := destroyLifetimeScope(attemptCtx, g.scope)
		cancel()
		if err == nil {
			g.scopeDestroyed = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *lifetimeGuardian) releaseScopeUntilDone(ctx context.Context) error {
	if g.scope == nil || g.scopeReleased {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.scopeReleased {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := releaseLifetimeScope(attemptCtx, g.scope)
		cancel()
		if err == nil {
			g.scopeReleased = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *lifetimeGuardian) releaseOwnerUntilDone(ctx context.Context) error {
	if g.owner == nil || g.ownerReleased {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.ownerReleased {
		if err := g.owner.Release(); err == nil {
			g.ownerReleased = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *lifetimeGuardian) refuseTerminalReplay(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if g.options.ownerKind == task.LifetimeOwnerTask {
		reader, ok := g.options.records.(lifetimeTaskLaunchRefusalReader)
		if !ok {
			return fmt.Errorf("%w: task lifetime records do not expose launch refusal", errLifetimeGuardianInvalid)
		}
		refused, err := reader.TaskLaunchRefusedContext(ctx)
		if err != nil {
			return err
		}
		if refused {
			return task.ErrTerminalTask
		}
	}
	completion, completionErr := g.options.records.ReadLifetimeWorkerWaitContext(ctx)
	if completionErr == nil && task.LifetimeWorkerWaitProvesAbsence(completion) {
		return task.ErrTerminalTask
	}
	if completionErr != nil && !task.RecordAbsent(completionErr) {
		return fmt.Errorf("read recovered lifetime completion before launch: %w", completionErr)
	}
	if err := g.refuseAuxiliaryUnlaunchedReplay(ctx); err != nil {
		return err
	}
	record, err := g.options.records.ReadLifetimeTerminationContext(ctx)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lifetime termination before launch: %w", err)
	}
	if record == nil || !record.ScopeEmpty {
		return fmt.Errorf("%w: termination reader returned invalid record", task.ErrEvidenceFault)
	}
	return task.ErrTerminalTask
}

func (g *lifetimeGuardian) refuseAuxiliaryUnlaunchedReplay(ctx context.Context) error {
	if g.options.ownerKind != task.LifetimeOwnerWorkspace && g.options.ownerKind != task.LifetimeOwnerInspection {
		return nil
	}
	reader, ok := g.options.records.(lifetimeUnlaunchedReader)
	if !ok {
		return fmt.Errorf("%w: auxiliary lifetime records do not expose the unlaunched reader", errLifetimeGuardianInvalid)
	}
	unlaunched, err := reader.ReadLifetimeUnlaunchedContext(ctx)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lifetime unlaunched fence before launch: %w", err)
	}
	if unlaunched == nil {
		return fmt.Errorf("%w: unlaunched reader returned nil record", task.ErrEvidenceFault)
	}
	return task.ErrLifetimeUnlaunched
}

func (g *lifetimeGuardian) acquireRunOwner(ctx context.Context) (lifetimeGuardianOwner, error) {
	owner, err := g.options.acquireOwner(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lifetime owner: %w", err)
	}
	if owner == nil {
		return nil, fmt.Errorf("%w: owner acquisition returned nil lease", errLifetimeGuardianInvalid)
	}
	g.owner = owner
	return owner, nil
}

func (g *lifetimeGuardian) prepareRunScope(ctx context.Context) (string, lifetimeGuardianScope, error) {
	bootID, err := g.options.bootID()
	if err != nil {
		return "", nil, fmt.Errorf("read lifetime boot identity: %w", err)
	}
	if bootID == "" {
		return "", nil, fmt.Errorf("%w: empty lifetime boot identity", errLifetimeGuardianInvalid)
	}
	plan, err := g.options.planFactory(g.options.rootID, g.options.ownerKind, g.options.taskID)
	if err != nil {
		return "", nil, fmt.Errorf("plan lifetime scope: %w", err)
	}
	if err = g.recordIntent(ctx, plan, bootID); err != nil {
		return "", nil, err
	}
	scope, err := g.options.scopeFactory(plan)
	if errors.Is(err, lifetime.ErrScopeExists) {
		recoveryCtx, cancel := g.recoveryContext(ctx)
		recoveryErr := g.options.reconcileOwned(recoveryCtx)
		cancel()
		if recoveryErr != nil {
			return "", nil, fmt.Errorf("reconcile existing lifetime scope: %w", recoveryErr)
		}
		if terminalErr := g.refuseTerminalReplay(ctx); terminalErr != nil {
			return "", nil, terminalErr
		}
		scope, err = g.options.scopeFactory(plan)
	}
	if err != nil {
		return "", nil, fmt.Errorf("create lifetime scope: %w", err)
	}
	if scope == nil {
		return "", nil, fmt.Errorf("%w: scope factory returned nil scope", errLifetimeGuardianInvalid)
	}
	return bootID, scope, nil
}

func (g *lifetimeGuardian) bindAndRecordRunScope(ctx context.Context, bootID string) error {
	if err := g.scope.BindOwnerProcess(); err != nil {
		cleanupCtx, cancel := g.recoveryContext(ctx)
		cleanupErr := g.resolveCreatedScopeFailure(cleanupCtx, bootID)
		cancel()
		return errors.Join(fmt.Errorf("bind lifetime scope owner: %w", err), cleanupErr)
	}
	if err := g.recordRegistration(ctx, bootID); err != nil {
		cleanupCtx, cancel := g.recoveryContext(ctx)
		cleanupErr := g.resolveCreatedScopeFailure(cleanupCtx, bootID)
		cancel()
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// resolveCreatedScopeFailure is the sole setup-failure resolver. Its caller
// retains the lifetime-owner lease. A scope is destroyed without a terminal
// record only after a fresh registration read proves that this exact scope was
// never published.
func (g *lifetimeGuardian) resolveCreatedScopeFailure(ctx context.Context, bootID string) error {
	registration, err := g.options.records.ReadLifetimeRegistrationContext(ctx)
	if task.RecordAbsent(err) {
		return g.disposeUnregisteredScope(ctx)
	}
	if err != nil {
		return fmt.Errorf("read lifetime registration after write failure: %w", err)
	}
	if !g.registrationMatchesScope(registration, bootID) {
		disposeErr := g.disposeUnregisteredScope(ctx)
		return errors.Join(task.ErrIdentityMismatch, disposeErr)
	}
	if err = g.recordRegistration(ctx, bootID); err != nil {
		return fmt.Errorf("acknowledge lifetime registration after setup failure: %w", err)
	}
	return g.terminateAndRecord(ctx, task.LifetimeTerminationRecovery)
}

func (g *lifetimeGuardian) disposeUnregisteredScope(ctx context.Context) error {
	err := disposeUnregisteredLifetimeScope(ctx, g.scope, g.scope.Reference().Assurance())
	if err == nil {
		g.unregisteredDisposed = true
		g.scopeDestroyed = true
	}
	return err
}

func (g *lifetimeGuardian) registrationMatchesScope(registration *task.LifetimeRegistrationRecord, bootID string) bool {
	if registration == nil || g.scope == nil {
		return false
	}
	ref := g.scope.Reference()
	return registration.RootID == g.options.rootID &&
		registration.TaskID == g.options.taskID &&
		registration.OwnerKind == g.options.ownerKind &&
		registration.ScopeKind == string(ref.Kind()) &&
		registration.Assurance == string(ref.Assurance()) &&
		registration.ScopeID == ref.Identifier() &&
		registration.BootID == bootID &&
		registration.Deadline == g.options.deadline.UTC().Format(time.RFC3339Nano)
}

func (g *lifetimeGuardian) startAndRecordWorker(ctx context.Context, reader *lifetimeGuardianReader, bootID string) (<-chan error, error) {
	if err := g.options.startWorker(g.options.worker); err != nil {
		stopErr := g.stopReader(ctx, reader)
		recoveryErr := g.recoverWithCause(ctx, fmt.Errorf("start lifetime worker: %w", err))
		return nil, errors.Join(err, stopErr, recoveryErr)
	}
	g.workerStarted = true
	if g.options.worker.Process != nil {
		g.workerPID = g.options.worker.Process.Pid
	}
	workerRef, err := g.options.observeWorker(g.options.worker)
	if err == nil {
		g.workerPID = workerRef.PID()
		err = g.scope.TrackOwnerProcess(workerRef)
	}
	waitDone := make(chan error, 1)
	g.workerWait = waitDone
	go func() {
		waitDone <- g.options.waitWorker(g.options.worker)
	}()
	if err == nil {
		err = g.persistWorkerIdentity(context.WithoutCancel(ctx), workerRef, bootID)
	}
	if err == nil {
		return waitDone, nil
	}
	cleanupCtx, cancel := g.cleanupContext(ctx)
	stopErr := g.stopReaderWithContext(cleanupCtx, reader)
	cancel()
	_, waitErr := g.awaitWorkerWait(context.WithoutCancel(ctx))
	waitRecordErr := g.persistWorkerWait(context.WithoutCancel(ctx))
	cleanupCtx, cancel = g.cleanupContext(ctx)
	defer cancel()
	recoveryErr := g.recoverWithCause(cleanupCtx, fmt.Errorf("record lifetime worker: %w", err))
	return nil, errors.Join(err, stopErr, waitErr, waitRecordErr, recoveryErr)
}

// persistWorkerIdentity retains native authority until the started worker's
// create-once identity is durably acknowledged. RecordLifetimeWorker supports
// exact replay, so retrying also resolves a write that committed before its
// durability barrier reported an error. The worker cannot receive its hello
// acknowledgement until this stage succeeds.
func (g *lifetimeGuardian) persistWorkerIdentity(ctx context.Context, ref lifetime.ProcessRef, bootID string) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if err := g.recordWorker(ctx, ref, bootID); err == nil {
			return nil
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.waitRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *lifetimeGuardian) beginRun() bool {
	g.runMu.Lock()
	defer g.runMu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (g *lifetimeGuardian) endRun() {
	g.runMu.Lock()
	g.running = false
	g.runMu.Unlock()
}

func (g *lifetimeGuardian) recordIntent(ctx context.Context, plan lifetime.Plan, bootID string) error {
	record, err := g.options.records.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{
		ScopeKind: string(plan.Kind()),
		Assurance: string(plan.Assurance()),
		ScopePlan: plan.Identifier(),
		BootID:    bootID,
		Deadline:  g.options.deadline,
	})
	if err != nil {
		return fmt.Errorf("record lifetime intent: %w", err)
	}
	if record == nil || record.TaskID != g.options.taskID || record.ScopePlan != plan.Identifier() {
		return fmt.Errorf("%w: intent writer returned invalid record", errLifetimeGuardianInvalid)
	}
	return nil
}

func (g *lifetimeGuardian) recordRegistration(ctx context.Context, bootID string) error {
	ref := g.scope.Reference()
	record, err := g.options.records.RecordLifetimeRegistrationContext(ctx, taskdir.LifetimeRegistrationFacts{
		ScopeKind: string(ref.Kind()),
		Assurance: string(ref.Assurance()),
		ScopeID:   ref.Identifier(),
		BootID:    bootID,
		Deadline:  g.options.deadline,
	})
	if err != nil {
		return fmt.Errorf("record lifetime registration: %w", err)
	}
	if !g.registrationMatchesScope(record, bootID) {
		return fmt.Errorf("%w: registration writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.registrationRecorded = true
	return nil
}

func (g *lifetimeGuardian) recordWorker(ctx context.Context, ref lifetime.ProcessRef, bootID string) error {
	record, err := g.options.records.RecordLifetimeWorkerContext(ctx, taskdir.LifetimeWorkerFacts{
		PID:          ref.PID(),
		ProcessStart: ref.StartToken(),
		BootID:       bootID,
	})
	if err != nil {
		return err
	}
	if record == nil || record.TaskID != g.options.taskID || record.PID != ref.PID() || record.ProcessStart != ref.StartToken() {
		return fmt.Errorf("%w: worker writer returned invalid record", errLifetimeGuardianInvalid)
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return fmt.Errorf("marshal lifetime worker record: %w", err)
	}
	g.workerPID = record.PID
	g.workerSHA256 = task.ComputeSHA256(data)
	return nil
}

// recordWorkerWait persists the managed-worker completion disposition. The
// candidate is frozen on first observation so retries cannot rewrite its
// timestamp or classification. A worker that never started has no process
// identity; a started worker requires its exact child Wait observation.
func (g *lifetimeGuardian) recordWorkerWait(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.workerStarted && (!g.workerWaitObserved || g.workerPID <= 0) {
		return fmt.Errorf("%w: exact worker Wait is unavailable", errLifetimeGuardianInvalid)
	}
	g.freezeWorkerWaitFacts()
	record, err := g.options.records.RecordLifetimeWorkerWaitContext(ctx, *g.workerWaitFacts)
	if err != nil {
		return fmt.Errorf("record lifetime worker Wait: %w", err)
	}
	if !g.workerWaitRecordMatches(record) {
		return fmt.Errorf("%w: worker Wait writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.workerWaitRecorded = true
	return nil
}

// persistWorkerWait retains the lifetime owner and scope until the frozen,
// exact child Wait becomes durable. Once Wait has been consumed there is no
// second process owner that can reconstruct it, so a transient storage failure
// must delay release rather than turn queue state into substitute evidence.
func (g *lifetimeGuardian) persistWorkerWait(ctx context.Context) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := g.recordWorkerWait(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.waitRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func waitLifetimeWorkerRecordRetry(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	timer := time.NewTimer(defaultLifetimeWorkerWaitRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resolveLifetimeWorkerRecordRetry(configured func(context.Context) error) func(context.Context) error {
	if configured != nil {
		return configured
	}
	return waitLifetimeWorkerRecordRetry
}

func waitLifetimeFinalizationBackoff(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return context.Canceled
	}
	if delay <= 0 {
		delay = defaultLifetimeFinalizationBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resolveLifetimeFinalizationBackoff(configured func(context.Context, time.Duration) error) func(context.Context, time.Duration) error {
	if configured != nil {
		return configured
	}
	return waitLifetimeFinalizationBackoff
}

func nextLifetimeFinalizationBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return defaultLifetimeFinalizationBackoff
	}
	if current >= maxLifetimeFinalizationBackoff/2 {
		return maxLifetimeFinalizationBackoff
	}
	return current * 2
}

// waitFinalizationBackoff treats an injected wait failure as a test-hook
// failure, never as permission to abandon an owned scope. The production
// default is already bounded; the fallback also keeps a faulty test hook from
// turning a retry loop into a busy loop.
func (g *lifetimeGuardian) waitFinalizationBackoff(ctx context.Context, delay time.Duration) {
	if err := g.options.finalizationBackoff(ctx, delay); err == nil {
		return
	}
	waitResidentLifetimeFinalizationBackoff(delay)
}

func (g *lifetimeGuardian) waitFinalizationRetry(ctx context.Context, delay time.Duration, retry func(context.Context) error) {
	if retry != nil && retry(ctx) != nil {
		waitResidentLifetimeFinalizationBackoff(delay)
		return
	}
	g.waitFinalizationBackoff(ctx, delay)
}

// waitResidentLifetimeFinalizationBackoff is the non-failing fallback for an
// injected retry wait. Finalization cannot return merely because its wait hook
// failed, and the bounded delay prevents a faulty hook from creating a busy
// loop while the guardian still owns native authority.
func waitResidentLifetimeFinalizationBackoff(delay time.Duration) {
	if delay <= 0 {
		delay = defaultLifetimeFinalizationBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}

func (g *lifetimeGuardian) freezeWorkerWaitFacts() {
	if g.workerWaitFacts != nil {
		return
	}
	if !g.workerStarted {
		g.workerWaitFacts = &taskdir.LifetimeWorkerWaitFacts{
			Result:     task.LifetimeWorkerWaitNotStarted,
			ObservedAt: g.options.now().UTC(),
		}
		return
	}
	result, exitCode := classifyLifetimeWorkerWait(g.options.worker, g.workerWaitErr)
	g.workerWaitFacts = &taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: g.workerSHA256,
		ChildPID:     g.workerPID,
		Result:       result,
		ExitCode:     exitCode,
		ObservedAt:   g.options.now().UTC(),
	}
}

func (g *lifetimeGuardian) workerWaitRecordMatches(record *task.LifetimeWorkerWaitRecord) bool {
	return record != nil && task.ValidateLifetimeWorkerWaitRecord(record) == nil &&
		record.TaskID == g.options.taskID && record.ChildPID == g.workerWaitFacts.ChildPID &&
		record.WorkerSHA256 == g.workerWaitFacts.WorkerSHA256 && record.Result == g.workerWaitFacts.Result &&
		sameOptionalExitCode(record.ExitCode, g.workerWaitFacts.ExitCode) &&
		record.ObservedAt == g.workerWaitFacts.ObservedAt.Format(time.RFC3339Nano)
}

func classifyLifetimeWorkerWait(cmd *exec.Cmd, waitErr error) (task.LifetimeWorkerWaitResult, *int) {
	if waitErr == nil {
		exitCode := 0
		return task.LifetimeWorkerWaitSuccess, &exitCode
	}
	processState := cmd.ProcessState
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if exitErr != nil && exitErr.ProcessState != nil {
			processState = exitErr.ProcessState
		}
		if processState != nil {
			exitCode := processState.ExitCode()
			return task.LifetimeWorkerWaitExitError, &exitCode
		}
	}
	if processState != nil {
		exitCode := processState.ExitCode()
		return task.LifetimeWorkerWaitError, &exitCode
	}
	return task.LifetimeWorkerWaitError, nil
}

func sameOptionalExitCode(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (g *lifetimeGuardian) serve(ctx context.Context, reader *lifetimeGuardianReader, waitDone <-chan error) lifetimeGuardianServeResult {
	deadlineTimer, deadlineEvents := lifetimeGuardianDeadlineTimer(g.options.deadline)
	if deadlineTimer != nil {
		defer deadlineTimer.Stop()
	}
	stopTimer, stopEvents := lifetimeGuardianStopTimer(g.options.userStop, g.options.stopPoll)
	if stopTimer != nil {
		defer stopTimer.Stop()
	}
	for {
		if result, handled := g.tryServeReadEvent(ctx, reader); handled {
			if result.continueServing {
				continue
			}
			return result
		}
		if g.deadlineReached() {
			return g.recoverForCause(ctx, errLifetimeGuardianDeadline, task.LifetimeTerminationBudget)
		}
		result := g.waitForServeEvent(ctx, reader, waitDone, deadlineEvents, stopEvents)
		if result.continueServing {
			continue
		}
		return result
	}
}

func (g *lifetimeGuardian) tryServeReadEvent(ctx context.Context, reader *lifetimeGuardianReader) (lifetimeGuardianServeResult, bool) {
	select {
	case event, ok := <-reader.events:
		return g.handleReadEvent(ctx, event, ok), true
	default:
		return lifetimeGuardianServeResult{}, false
	}
}

func (g *lifetimeGuardian) waitForServeEvent(
	ctx context.Context,
	reader *lifetimeGuardianReader,
	waitDone <-chan error,
	deadlineEvents <-chan time.Time,
	stopEvents <-chan time.Time,
) lifetimeGuardianServeResult {
	select {
	case event, ok := <-reader.events:
		return g.handleReadEvent(ctx, event, ok)
	case waitErr := <-waitDone:
		return g.handleWorkerExit(ctx, waitErr)
	case <-deadlineEvents:
		return g.recoverForCause(ctx, errLifetimeGuardianDeadline, task.LifetimeTerminationBudget)
	case <-stopEvents:
		return g.handleUserStop(ctx)
	case <-ctx.Done():
		cause := fmt.Errorf("lifetime guardian context ended: %w", ctx.Err())
		return g.recoverForCause(ctx, cause, task.LifetimeTerminationRecovery)
	}
}

func (g *lifetimeGuardian) handleWorkerExit(ctx context.Context, waitErr error) lifetimeGuardianServeResult {
	g.workerWaitObserved = true
	g.workerWaitErr = waitErr
	cause := errLifetimeGuardianWorkerLost
	if waitErr != nil {
		cause = errors.Join(cause, waitErr)
	}
	return g.recoverForCause(ctx, cause, task.LifetimeTerminationRecovery)
}

func (g *lifetimeGuardian) handleUserStop(ctx context.Context) lifetimeGuardianServeResult {
	requested, err := g.options.userStop()
	if err != nil {
		return g.recoverForCause(ctx, fmt.Errorf("read durable user stop request: %w", err), task.LifetimeTerminationRecovery)
	}
	if !requested {
		return lifetimeGuardianServeResult{continueServing: true}
	}
	return g.recoverForCause(ctx, errLifetimeGuardianStopRequested, task.LifetimeTerminationRequested)
}

func lifetimeGuardianStopTimer(check func() (bool, error), interval time.Duration) (*time.Ticker, <-chan time.Time) {
	if check == nil {
		return nil, nil
	}
	ticker := time.NewTicker(interval)
	return ticker, ticker.C
}

func (g *lifetimeGuardian) handleReadEvent(ctx context.Context, event lifetimeGuardianReadResult, ok bool) lifetimeGuardianServeResult {
	if !ok {
		return g.recoverForCause(ctx, errLifetimeGuardianWorkerLost, task.LifetimeTerminationRecovery)
	}
	if event.err != nil {
		if g.deadlineReached() {
			return g.recoverForCause(ctx, errLifetimeGuardianDeadline, task.LifetimeTerminationBudget)
		}
		if ctx.Err() != nil {
			return g.recoverForCause(ctx, fmt.Errorf("lifetime guardian context ended: %w", ctx.Err()), task.LifetimeTerminationRecovery)
		}
		cause := fmt.Errorf("%w: control channel: %w", errLifetimeGuardianWorkerLost, event.err)
		return g.recoverForCause(ctx, cause, task.LifetimeTerminationRecovery)
	}
	terminal, err := g.handleMessage(ctx, event.message)
	if err == nil {
		return lifetimeGuardianServeResult{continueServing: !terminal}
	}
	if terminal {
		return lifetimeGuardianServeResult{err: err}
	}
	recovery := g.recoverWithCause(ctx, err)
	return lifetimeGuardianServeResult{err: recovery}
}

func (g *lifetimeGuardian) handleMessage(ctx context.Context, message lifetimeMessage) (bool, error) {
	switch message.Type {
	case lifetimeMessageHello:
		return false, g.handleHello(ctx, message)
	case lifetimeMessageBegin:
		return g.handleBegin(ctx, message)
	case lifetimeMessageStarted:
		return false, g.handleStarted(ctx, message)
	case lifetimeMessageFailed:
		return false, g.handleFailed(ctx, message)
	case lifetimeMessageFinish:
		return false, g.handleFinish(ctx, message)
	case lifetimeMessageStop:
		return g.handleTermination(ctx, message, task.LifetimeTerminationBudget)
	case lifetimeMessageClose:
		return g.handleTermination(ctx, message, task.LifetimeTerminationCompleted)
	default:
		return false, g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
}

func (g *lifetimeGuardian) handleHello(ctx context.Context, message lifetimeMessage) error {
	if g.helloReceived {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.helloReceived = true
	return g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) handleBegin(ctx context.Context, message lifetimeMessage) (bool, error) {
	if err := g.requireReady(message, true); err != nil {
		return false, g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	if g.deadlineReached() {
		cleanupCtx, cancel := g.cleanupContext(ctx)
		terminationErr := g.terminateAndRecord(cleanupCtx, task.LifetimeTerminationBudget)
		cancel()
		if terminationErr == nil {
			g.terminal = true
			return true, g.reject(ctx, message, lifetimeGuardianCodeDeadline)
		}
		replyErr := g.reject(ctx, message, lifetimeGuardianCodeTermination)
		return false, errors.Join(terminationErr, replyErr)
	}
	if message.Sequence > task.MaxLifetimeLaunches || message.Sequence != g.nextSequence+1 {
		return false, g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.nextSequence = message.Sequence
	g.authorizedSequence = message.Sequence
	return false, g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) handleStarted(ctx context.Context, message lifetimeMessage) error {
	if err := g.requireReady(message, false); err != nil || g.authorizedSequence != message.Sequence {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	controlCtx, cancel := g.workContext(ctx)
	defer cancel()
	launchRef, err := observeLifetimeLaunch(controlCtx, g.scope, message.PID)
	if err != nil {
		replyErr := g.reject(ctx, message, lifetimeGuardianCodeStarted)
		return errors.Join(fmt.Errorf("observe started lifetime launch: %w", err), replyErr)
	}
	g.launchRefs = appendLifetimeLaunchRef(g.launchRefs, launchRef)
	record, err := g.options.records.RecordLifetimeLaunchContext(controlCtx, taskdir.LifetimeLaunchFacts{
		Sequence:     message.Sequence,
		PID:          launchRef.PID(),
		ProcessStart: launchRef.StartToken(),
		PGID:         launchRef.PGID(),
	})
	if err != nil {
		return g.rejectStartedLaunch(ctx, message, fmt.Errorf("record lifetime launch: %w", err))
	}
	if record == nil || record.Sequence != message.Sequence || record.PID != message.PID {
		return g.rejectStartedLaunch(ctx, message, errLifetimeGuardianLaunchRecord)
	}
	g.authorizedSequence = 0
	g.activeSequence = message.Sequence
	return g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) rejectStartedLaunch(ctx context.Context, message lifetimeMessage, cause error) error {
	controlCtx, cancel := g.cleanupContext(ctx)
	cleanupErr := finalizeLifetimeScope(controlCtx, g.scope, g.launchRefs)
	cancel()
	if cleanupErr != nil {
		return errors.Join(cause, fmt.Errorf("clean up unrecorded lifetime launch: %w", cleanupErr))
	}
	// The guardian completed cleanup while it still owns the scope. Clearing
	// authorization prevents recovery from waiting for worker-side cleanup a
	// second time before it records the terminal transition.
	g.authorizedSequence = 0
	return errors.Join(cause, g.reject(ctx, message, lifetimeGuardianCodeStarted))
}

func (g *lifetimeGuardian) handleFailed(ctx context.Context, message lifetimeMessage) error {
	if err := g.requireReady(message, false); err != nil || g.authorizedSequence != message.Sequence {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.authorizedSequence = 0
	return g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) handleFinish(ctx context.Context, message lifetimeMessage) error {
	if err := g.requireReady(message, false); err != nil || g.activeSequence != message.Sequence {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	controlCtx, cancel := g.workContext(ctx)
	defer cancel()
	if err := g.ensureScopeEmpty(controlCtx, g.launchRefs); err != nil {
		replyErr := g.reject(ctx, message, lifetimeGuardianCodeFinish)
		return errors.Join(fmt.Errorf("finish lifetime launch %d: %w", message.Sequence, err), replyErr)
	}
	g.activeSequence = 0
	return g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) handleTermination(ctx context.Context, message lifetimeMessage, cause string) (bool, error) {
	if err := g.requireReady(message, false); err != nil {
		return false, g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.fenced = true
	controlCtx, cancel := g.terminationContext(ctx, cause)
	defer cancel()
	if err := g.terminateAndRecord(controlCtx, cause); err != nil {
		replyErr := g.reject(ctx, message, lifetimeGuardianCodeTermination)
		return false, errors.Join(err, replyErr)
	}
	g.terminal = true
	return true, g.reply(ctx, message, true, "")
}

func (g *lifetimeGuardian) requireReady(message lifetimeMessage, begin bool) error {
	if !g.helloReceived || g.terminal || g.fenced {
		return errLifetimeGuardianOrder
	}
	if begin {
		if g.authorizedSequence != 0 || g.activeSequence != 0 {
			return errLifetimeGuardianOrder
		}
		return nil
	}
	if message.Type == lifetimeMessageStarted || message.Type == lifetimeMessageFailed {
		if g.authorizedSequence == 0 || g.activeSequence != 0 {
			return errLifetimeGuardianOrder
		}
	}
	if message.Type == lifetimeMessageFinish && (g.authorizedSequence != 0 || g.activeSequence == 0) {
		return errLifetimeGuardianOrder
	}
	return nil
}

func (g *lifetimeGuardian) terminateAndRecord(ctx context.Context, cause string) error {
	g.fenced = true
	if g.terminationRecorded {
		return nil
	}
	if err := g.freezeTerminationCause(cause); err != nil {
		return err
	}
	if !g.registrationRecorded {
		return fmt.Errorf("%w: registration is not durable", errLifetimeGuardianInvalid)
	}
	if !g.terminationEmptyProven {
		if err := g.fencePendingStart(ctx); err != nil {
			return fmt.Errorf("fence pending lifetime start: %w", err)
		}
		g.authorizedSequence = 0
		if err := g.ensureScopeEmpty(ctx, g.launchRefs); err != nil {
			return fmt.Errorf("terminate lifetime scope: %w", err)
		}
		g.terminationEmptyProven = true
	}
	return g.recordTermination(ctx)
}

func (g *lifetimeGuardian) recordTermination(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	record, err := g.options.records.RecordLifetimeTerminationContext(ctx, g.terminationCause)
	if err != nil {
		existing, readErr := g.options.records.ReadLifetimeTerminationContext(ctx)
		if readErr == nil && g.terminationRecordMatches(existing) {
			g.activeSequence = 0
			g.terminationRecorded = true
			return nil
		}
		if task.RecordAbsent(readErr) {
			readErr = nil
		}
		return errors.Join(fmt.Errorf("record lifetime termination: %w", err), readErr)
	}
	if !g.terminationRecordMatches(record) {
		return fmt.Errorf("%w: termination writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.activeSequence = 0
	g.terminationRecorded = true
	return nil
}

func (g *lifetimeGuardian) freezeTerminationCause(cause string) error {
	switch cause {
	case task.LifetimeTerminationCompleted,
		task.LifetimeTerminationBudget,
		task.LifetimeTerminationRequested,
		task.LifetimeTerminationRecovery:
	default:
		return fmt.Errorf("%w: invalid termination cause", errLifetimeGuardianInvalid)
	}
	if g.terminationCause == "" {
		g.terminationCause = cause
	}
	return nil
}

func (g *lifetimeGuardian) terminationRecordMatches(record *task.LifetimeTerminationRecord) bool {
	return record != nil && record.ScopeEmpty && record.Cause == g.terminationCause
}

func (g *lifetimeGuardian) persistTermination(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if g.terminationRecorded {
			return nil
		}
		if g.terminationEmptyProven {
			// Empty-scope proof is durable progress. Once it succeeds, only
			// termination publication may be retried; repeating native proof
			// could mutate ownership after the scope has already been fenced.
			attemptCtx, cancel := g.cleanupContext(ctx)
			err := g.terminateAndRecord(attemptCtx, g.terminationCause)
			cancel()
			if err == nil {
				return nil
			}
			g.waitFinalizationBackoff(ctx, backoff)
			backoff = nextLifetimeFinalizationBackoff(backoff)
			continue
		}

		// A proof batch is intentionally bounded. A fresh cleanup context is
		// created for every attempt, and a new batch is separated by backoff.
		for attempt := 0; attempt < lifetimeTerminationProofAttempts; attempt++ {
			attemptCtx, cancel := g.cleanupContext(ctx)
			err := g.terminateAndRecord(attemptCtx, g.terminationCause)
			cancel()
			if err == nil {
				return nil
			}
			if g.terminationEmptyProven {
				break
			}
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *lifetimeGuardian) fencePendingStart(ctx context.Context) error {
	if g.pendingStartFenced {
		return nil
	}
	if g.authorizedSequence == 0 {
		g.pendingStartFenced = true
		return nil
	}
	if g.workerWaitObserved {
		return g.verifyPendingStartFence(ctx)
	}
	if g.workerWait == nil {
		return errors.New("task lifetime guardian: worker wait is unavailable for pending start")
	}
	// Closing the private control channel prevents a healthy worker from
	// completing the pending handoff. The worker owns the exact command Wait
	// and cleans a started-but-unacknowledged command through its scope before
	// exiting. Killing only the worker would leave a provider process group
	// alive and would violate the lifetime owner's stopping boundary.
	if g.codec == nil {
		return errLifetimeGuardianInvalid
	}
	if err := g.codec.Close(); err != nil {
		return fmt.Errorf("close lifetime control channel: %w", err)
	}
	observed, err := g.awaitWorkerWait(ctx)
	if !observed {
		return err
	}
	if err := g.verifyPendingStartFence(ctx); err != nil {
		return err
	}
	g.pendingStartFenced = true
	return nil
}

func (g *lifetimeGuardian) verifyPendingStartFence(ctx context.Context) error {
	if g.scope == nil {
		return fmt.Errorf("%w: pending-start scope is unavailable", lifetime.ErrUnverifiableLaunch)
	}
	empty, err := g.scope.PendingStartEmptyContext(ctx)
	if err != nil {
		return fmt.Errorf("prove pending lifetime start is empty: %w", err)
	}
	if empty {
		return nil
	}
	if err = finalizeLifetimeScope(ctx, g.scope, g.launchRefs); err != nil {
		return fmt.Errorf("terminate pending lifetime start: %w", err)
	}
	return nil
}

func (g *lifetimeGuardian) tryObserveWorkerWait() (bool, error) {
	if g.workerWaitObserved {
		return true, g.workerWaitErr
	}
	if g.workerWait == nil {
		return false, nil
	}
	select {
	case err := <-g.workerWait:
		g.workerWaitObserved = true
		g.workerWaitErr = err
		return true, err
	default:
		return false, nil
	}
}

func (g *lifetimeGuardian) awaitWorkerWait(ctx context.Context) (bool, error) {
	if observed, err := g.tryObserveWorkerWait(); observed {
		return true, err
	}
	if g.workerWait == nil {
		return false, errors.New("task lifetime guardian: worker wait is unavailable")
	}
	select {
	case err := <-g.workerWait:
		g.workerWaitObserved = true
		g.workerWaitErr = err
		return true, err
	case <-ctx.Done():
		return false, fmt.Errorf("wait for lifetime worker exit: %w", ctx.Err())
	}
}

func (g *lifetimeGuardian) ensureScopeEmpty(ctx context.Context, refs []lifetime.LaunchRef) error {
	return finalizeLifetimeScope(ctx, g.scope, refs)
}

func observeLifetimeEmpty(ctx context.Context, scope lifetimeFinalizationScope, refs []lifetime.LaunchRef) (bool, error) {
	if ctx == nil {
		return false, lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return scope.EmptyContext(ctx, refs)
}

func (g *lifetimeGuardian) reply(ctx context.Context, message lifetimeMessage, accepted bool, code string) error {
	if g.codec == nil {
		return errLifetimeGuardianInvalid
	}
	controlCtx, cancel := g.replyContext(ctx, message)
	defer cancel()
	return replyLifetimeWithContext(controlCtx, g.codec, message, accepted, code)
}

func (g *lifetimeGuardian) reject(ctx context.Context, message lifetimeMessage, code string) error {
	if code == "" {
		return errLifetimeGuardianInvalid
	}
	return g.reply(ctx, message, false, code)
}

func (g *lifetimeGuardian) deadlineReached() bool {
	return !g.options.now().Before(g.options.deadline)
}

func (g *lifetimeGuardian) recoverForCause(ctx context.Context, cause error, terminationCause string) lifetimeGuardianServeResult {
	return lifetimeGuardianServeResult{err: g.recoverWithCauseAndKind(ctx, cause, terminationCause)}
}

func (g *lifetimeGuardian) recoverWithCause(ctx context.Context, cause error) error {
	return g.recoverWithCauseAndKind(ctx, cause, task.LifetimeTerminationRecovery)
}

func (g *lifetimeGuardian) recoverWithCauseAndKind(ctx context.Context, cause error, terminationCause string) error {
	if g.terminationRecorded {
		return cause
	}
	cleanupCtx, cancel := g.cleanupContext(ctx)
	defer cancel()
	return errors.Join(cause, g.terminateAndRecord(cleanupCtx, terminationCause))
}

func (g *lifetimeGuardian) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), g.options.cleanupTimeout)
}

func (g *lifetimeGuardian) recoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), lifetimeRecoveryTimeout)
}

// workContext keeps native start and finish proof under the immutable business
// deadline. cleanupContext is the sole shorter fallback after work has ended.
func (g *lifetimeGuardian) workContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return nil, func() {}
	}
	return context.WithDeadline(parent, g.options.deadline)
}

func (g *lifetimeGuardian) replyContext(parent context.Context, message lifetimeMessage) (context.Context, context.CancelFunc) {
	if message.Type == lifetimeMessageStop || g.deadlineReached() {
		return g.cleanupContext(parent)
	}
	return g.workContext(parent)
}

// terminationContext keeps a normal completed transition inside the original
// work budget. Stop and recovery remain independently bounded because they run
// after work has been fenced or its deadline has already elapsed.
func (g *lifetimeGuardian) terminationContext(parent context.Context, cause string) (context.Context, context.CancelFunc) {
	if cause == task.LifetimeTerminationCompleted && !g.deadlineReached() {
		return g.workContext(parent)
	}
	return g.cleanupContext(parent)
}

func (g *lifetimeGuardian) stopReader(parent context.Context, reader *lifetimeGuardianReader) error {
	cleanupCtx, cancel := g.cleanupContext(parent)
	defer cancel()
	return g.stopReaderWithContext(cleanupCtx, reader)
}

func (g *lifetimeGuardian) stopReaderWithContext(ctx context.Context, reader *lifetimeGuardianReader) error {
	reader.once.Do(func() {
		close(reader.stop)
	})
	closeErr := g.codec.Close()
	select {
	case <-reader.done:
		return closeErr
	case <-ctx.Done():
		return errors.Join(closeErr, ctx.Err())
	}
}

func destroyLifetimeScope(ctx context.Context, scope lifetimeGuardianScope) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return scope.DestroyEmpty()
}

type lifetimeReleaseScope interface {
	Release() error
}

func releaseLifetimeScope(ctx context.Context, scope lifetimeReleaseScope) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if scope == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var releaseErr error
	for attempt := 0; attempt < lifetimeScopeReleaseAttempts; attempt++ {
		if err := scope.Release(); err == nil {
			return nil
		} else {
			releaseErr = errors.Join(releaseErr, err)
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(releaseErr, err)
		}
	}
	return releaseErr
}

// releaseLifetimeScopeUncanceled ignores caller cancellation but remains
// synchronous. A caller holding the lifetime-owner lease therefore retains it
// until the local scope handle has been released.
func releaseLifetimeScopeUncanceled(parent context.Context, scope lifetimeReleaseScope) error {
	if parent == nil {
		return lifetime.ErrNilContext
	}
	return releaseLifetimeScope(context.WithoutCancel(parent), scope)
}

func observeLifetimeLaunch(ctx context.Context, scope lifetimeGuardianScope, pid int) (lifetime.LaunchRef, error) {
	if ctx == nil {
		return lifetime.LaunchRef{}, lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return lifetime.LaunchRef{}, err
	}
	ref, err := scope.ObserveStarted(pid)
	if err != nil {
		return lifetime.LaunchRef{}, err
	}
	if err = ctx.Err(); err != nil {
		return lifetime.LaunchRef{}, err
	}
	return ref, nil
}

func appendLifetimeLaunchRef(refs []lifetime.LaunchRef, candidate lifetime.LaunchRef) []lifetime.LaunchRef {
	for _, existing := range refs {
		if existing.PID() == candidate.PID() && existing.StartToken() == candidate.StartToken() && existing.PGID() == candidate.PGID() {
			return refs
		}
	}
	return append(refs, candidate)
}

func startLifetimeGuardianReader(codec *lifetimeCodec, receiveCtx context.Context) *lifetimeGuardianReader {
	reader := &lifetimeGuardianReader{
		events: make(chan lifetimeGuardianReadResult, 1),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
	}
	go func() {
		defer close(reader.done)
		defer close(reader.events)
		contextDone := make(chan struct{})
		defer close(contextDone)
		go func() {
			select {
			case <-receiveCtx.Done():
				if err := codec.Close(); err != nil {
					reader.mu.Lock()
					reader.err = err
					reader.mu.Unlock()
				}
			case <-contextDone:
			}
		}()
		for {
			message, err := receiveLifetimeGuardianMessage(receiveCtx, codec)
			reader.mu.Lock()
			closeErr := reader.err
			reader.mu.Unlock()
			if closeErr != nil {
				err = errors.Join(err, closeErr)
			}
			select {
			case reader.events <- lifetimeGuardianReadResult{message: message, err: err}:
			case <-reader.stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

func receiveLifetimeGuardianMessage(ctx context.Context, codec *lifetimeCodec) (lifetimeMessage, error) {
	if ctx == nil {
		return lifetimeMessage{}, lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return lifetimeMessage{}, err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return lifetimeMessage{}, errors.New("task lifetime guardian: receive context has no deadline")
	}
	if err := codec.conn.SetReadDeadline(deadline); err != nil {
		return lifetimeMessage{}, err
	}
	var message lifetimeMessage
	readErr := codec.readFrame(&message)
	clearErr := codec.conn.SetReadDeadline(time.Time{})
	if err := errors.Join(readErr, clearErr); err != nil {
		return lifetimeMessage{}, err
	}
	return message, validateLifetimeMessage(message)
}

func lifetimeGuardianDeadlineTimer(deadline time.Time) (*time.Timer, <-chan time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		closed := make(chan time.Time)
		close(closed)
		return nil, closed
	}
	timer := time.NewTimer(remaining)
	return timer, timer.C
}

func replyLifetimeWithContext(ctx context.Context, codec *lifetimeCodec, message lifetimeMessage, accepted bool, code string) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return errors.New("task lifetime guardian: reply context has no deadline")
	}
	if err := codec.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	reply := lifetimeReply{
		Version:  lifetimeProtocolVersion,
		Type:     message.Type,
		Sequence: message.Sequence,
		Accepted: accepted,
		Code:     code,
	}
	if accepted {
		reply.Code = ""
	}
	writeErr := codec.writeFrame(reply)
	clearErr := codec.conn.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, clearErr)
}
