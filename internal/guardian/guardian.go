package guardian

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Guardian owns one managed worker, its native scope handle, and the lifetime
// lease through durable finalization. Its mutable transition state is private.
type Guardian struct {
	options Options

	runMu   sync.Mutex
	running bool

	owner  Owner
	scope  Scope
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

// Run acquires the task lifetime owner, registers the strongest available
// scope, starts one worker, and then serves the private lifetime protocol.
func (g *Guardian) Run(ctx context.Context) (resultErr error) {
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
func (g *Guardian) runOwned(ctx context.Context) (resultErr error) {
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
	codec, err := newLifetimeCodec(g.options.Connection)
	if err != nil {
		cause := fmt.Errorf("create lifetime control codec: %w", err)
		return errors.Join(cause, g.recoverWithCause(ctx, cause))
	}
	g.codec = codec
	receiveCtx, cancelReceive := context.WithDeadline(ctx, g.options.Deadline)
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
func (g *Guardian) recoverExpiredStart(ctx context.Context) error {
	intent, err := OptionalIntent(g.options.Records.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return errors.Join(errLifetimeGuardianDeadline, err)
	}
	registration, err := OptionalRegistration(g.options.Records.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return errors.Join(errLifetimeGuardianDeadline, err)
	}
	if intent == nil && registration == nil {
		return errLifetimeGuardianDeadline
	}
	recoveryCtx, cancel := g.recoveryContext(ctx)
	defer cancel()
	return errors.Join(errLifetimeGuardianDeadline, g.options.ReconcileOwned(recoveryCtx))
}

func (g *Guardian) refuseTerminalReplay(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if g.options.OwnerKind == task.LifetimeOwnerTask {
		reader, ok := g.options.Records.(lifetimeTaskLaunchRefusalReader)
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
	completion, completionErr := g.options.Records.ReadLifetimeWorkerWaitContext(ctx)
	if completionErr == nil && task.LifetimeWorkerWaitProvesAbsence(completion) {
		return task.ErrTerminalTask
	}
	if completionErr != nil && !task.RecordAbsent(completionErr) {
		return fmt.Errorf("read recovered lifetime completion before launch: %w", completionErr)
	}
	if err := g.refuseAuxiliaryUnlaunchedReplay(ctx); err != nil {
		return err
	}
	record, err := g.options.Records.ReadLifetimeTerminationContext(ctx)
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

func (g *Guardian) refuseAuxiliaryUnlaunchedReplay(ctx context.Context) error {
	if g.options.OwnerKind != task.LifetimeOwnerWorkspace && g.options.OwnerKind != task.LifetimeOwnerInspection {
		return nil
	}
	reader, ok := g.options.Records.(lifetimeUnlaunchedReader)
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

func (g *Guardian) acquireRunOwner(ctx context.Context) (Owner, error) {
	owner, err := g.options.AcquireOwner(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lifetime owner: %w", err)
	}
	if owner == nil {
		return nil, fmt.Errorf("%w: owner acquisition returned nil lease", errLifetimeGuardianInvalid)
	}
	g.owner = owner
	return owner, nil
}

func (g *Guardian) beginRun() bool {
	g.runMu.Lock()
	defer g.runMu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (g *Guardian) endRun() {
	g.runMu.Lock()
	g.running = false
	g.runMu.Unlock()
}
