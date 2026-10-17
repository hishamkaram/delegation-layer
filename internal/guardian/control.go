package guardian

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

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

func (g *Guardian) serve(ctx context.Context, reader *lifetimeGuardianReader, waitDone <-chan error) lifetimeGuardianServeResult {
	deadlineTimer, deadlineEvents := lifetimeGuardianDeadlineTimer(g.options.Deadline)
	if deadlineTimer != nil {
		defer deadlineTimer.Stop()
	}
	stopTimer, stopEvents := lifetimeGuardianStopTimer(g.options.UserStop, g.options.StopPoll)
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

func (g *Guardian) tryServeReadEvent(ctx context.Context, reader *lifetimeGuardianReader) (lifetimeGuardianServeResult, bool) {
	select {
	case event, ok := <-reader.events:
		return g.handleReadEvent(ctx, event, ok), true
	default:
		return lifetimeGuardianServeResult{}, false
	}
}

func (g *Guardian) waitForServeEvent(
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

func (g *Guardian) handleWorkerExit(ctx context.Context, waitErr error) lifetimeGuardianServeResult {
	g.workerWaitObserved = true
	g.workerWaitErr = waitErr
	cause := errLifetimeGuardianWorkerLost
	if waitErr != nil {
		cause = errors.Join(cause, waitErr)
	}
	return g.recoverForCause(ctx, cause, task.LifetimeTerminationRecovery)
}

func (g *Guardian) handleUserStop(ctx context.Context) lifetimeGuardianServeResult {
	requested, err := g.options.UserStop()
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

func (g *Guardian) handleReadEvent(ctx context.Context, event lifetimeGuardianReadResult, ok bool) lifetimeGuardianServeResult {
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

func (g *Guardian) handleMessage(ctx context.Context, message lifetimeMessage) (bool, error) {
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

func (g *Guardian) handleHello(ctx context.Context, message lifetimeMessage) error {
	if g.helloReceived {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.helloReceived = true
	return g.reply(ctx, message, true, "")
}

func (g *Guardian) handleBegin(ctx context.Context, message lifetimeMessage) (bool, error) {
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

func (g *Guardian) handleStarted(ctx context.Context, message lifetimeMessage) error {
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
	record, err := g.options.Records.RecordLifetimeLaunchContext(controlCtx, taskdir.LifetimeLaunchFacts{
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

func (g *Guardian) rejectStartedLaunch(ctx context.Context, message lifetimeMessage, cause error) error {
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

func (g *Guardian) handleFailed(ctx context.Context, message lifetimeMessage) error {
	if err := g.requireReady(message, false); err != nil || g.authorizedSequence != message.Sequence {
		return g.reject(ctx, message, lifetimeGuardianCodeInvalidOrder)
	}
	g.authorizedSequence = 0
	return g.reply(ctx, message, true, "")
}

func (g *Guardian) handleFinish(ctx context.Context, message lifetimeMessage) error {
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

func (g *Guardian) handleTermination(ctx context.Context, message lifetimeMessage, cause string) (bool, error) {
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

func (g *Guardian) requireReady(message lifetimeMessage, begin bool) error {
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

func (g *Guardian) reply(ctx context.Context, message lifetimeMessage, accepted bool, code string) error {
	if g.codec == nil {
		return errLifetimeGuardianInvalid
	}
	controlCtx, cancel := g.replyContext(ctx, message)
	defer cancel()
	return replyLifetimeWithContext(controlCtx, g.codec, message, accepted, code)
}

func (g *Guardian) reject(ctx context.Context, message lifetimeMessage, code string) error {
	if code == "" {
		return errLifetimeGuardianInvalid
	}
	return g.reply(ctx, message, false, code)
}

func (g *Guardian) stopReader(parent context.Context, reader *lifetimeGuardianReader) error {
	cleanupCtx, cancel := g.cleanupContext(parent)
	defer cancel()
	return g.stopReaderWithContext(cleanupCtx, reader)
}

func (g *Guardian) stopReaderWithContext(ctx context.Context, reader *lifetimeGuardianReader) error {
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
	readErr := readLifetimeFrame(codec, &message)
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
	writeErr := writeLifetimeFrame(codec, reply)
	clearErr := codec.conn.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, clearErr)
}
