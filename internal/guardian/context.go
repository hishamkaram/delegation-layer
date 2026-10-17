package guardian

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (g *Guardian) deadlineReached() bool {
	return !g.options.Now().Before(g.options.Deadline)
}

func (g *Guardian) recoverForCause(ctx context.Context, cause error, terminationCause string) lifetimeGuardianServeResult {
	return lifetimeGuardianServeResult{err: g.recoverWithCauseAndKind(ctx, cause, terminationCause)}
}

func (g *Guardian) recoverWithCause(ctx context.Context, cause error) error {
	return g.recoverWithCauseAndKind(ctx, cause, task.LifetimeTerminationRecovery)
}

func (g *Guardian) recoverWithCauseAndKind(ctx context.Context, cause error, terminationCause string) error {
	if g.terminationRecorded {
		return cause
	}
	cleanupCtx, cancel := g.cleanupContext(ctx)
	defer cancel()
	return errors.Join(cause, g.terminateAndRecord(cleanupCtx, terminationCause))
}

func (g *Guardian) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), g.options.CleanupTimeout)
}

func (g *Guardian) recoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), lifetimeRecoveryTimeout)
}

// workContext keeps native start and finish proof under the immutable business
// deadline. cleanupContext is the sole shorter fallback after work has ended.
func (g *Guardian) workContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return nil, func() {}
	}
	return context.WithDeadline(parent, g.options.Deadline)
}

func (g *Guardian) replyContext(parent context.Context, message lifetimeMessage) (context.Context, context.CancelFunc) {
	if message.Type == lifetimeMessageStop || g.deadlineReached() {
		return g.cleanupContext(parent)
	}
	return g.workContext(parent)
}

// terminationContext keeps a normal completed transition inside the original
// work budget. Stop and recovery remain independently bounded because they run
// after work has been fenced or its deadline has already elapsed.
func (g *Guardian) terminationContext(parent context.Context, cause string) (context.Context, context.CancelFunc) {
	if cause == task.LifetimeTerminationCompleted && !g.deadlineReached() {
		return g.workContext(parent)
	}
	return g.cleanupContext(parent)
}
