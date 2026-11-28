package guardian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (g *Guardian) deadlineReached() bool {
	return !g.options.Deadline.IsZero() && !g.options.Now().Before(g.options.Deadline)
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
	if g.options.Unlimited {
		return context.WithTimeout(parent, lifetimeControlTimeout)
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

// UnlimitedStartupTimeout bounds preparation when execution has no deadline.
const UnlimitedStartupTimeout = 30 * time.Second

var errLifetimeGuardianStartupDeadline = fmt.Errorf("task lifetime guardian: startup deadline reached: %w", context.DeadlineExceeded)

func (g *Guardian) startupExpired() bool {
	return g.options.Unlimited && !g.options.Now().Before(g.options.StartupDeadline)
}

func (g *Guardian) startupTimer() (*time.Timer, <-chan time.Time) {
	if !g.options.Unlimited || g.helloReceived {
		return nil, nil
	}
	return lifetimeGuardianDeadlineTimer(g.options.StartupDeadline)
}

func (g *Guardian) startupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if g.options.Unlimited {
		deadline := g.options.StartupDeadline
		if deadline.IsZero() {
			deadline = g.options.Now().Add(UnlimitedStartupTimeout)
		}
		return context.WithDeadline(parent, deadline)
	}
	return g.workContext(parent)
}

func (g *Guardian) receiveContext(parent context.Context) (context.Context, context.CancelFunc) {
	if g.options.Unlimited {
		return context.WithCancel(parent)
	}
	return context.WithDeadline(parent, g.options.Deadline)
}
