package app

import (
	"context"
	"sync"
	"time"
)

// controllableDeadlineContext lets a test place a command in a controlled
// pending state before delivering the same DeadlineExceeded signal that a real
// finite caller deadline would provide.
type controllableDeadlineContext struct {
	parent   context.Context
	deadline time.Time
	done     chan struct{}

	once sync.Once
	err  error
}

func newControllableDeadlineContext(parent context.Context, timeout time.Duration) (*controllableDeadlineContext, func()) {
	deadline := time.Now().Add(timeout)
	ctx := &controllableDeadlineContext{parent: parent, deadline: deadline, done: make(chan struct{})}
	timer := time.AfterFunc(timeout, func() { ctx.expire() })
	stopParent := context.AfterFunc(parent, func() { ctx.finish(parent.Err()) })
	return ctx, func() {
		timer.Stop()
		stopParent()
		ctx.finish(context.Canceled)
	}
}

func (ctx *controllableDeadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, true }

func (ctx *controllableDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *controllableDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return ctx.err
	default:
		return nil
	}
}

func (ctx *controllableDeadlineContext) Value(key any) any { return ctx.parent.Value(key) }

func (ctx *controllableDeadlineContext) expire() { ctx.finish(context.DeadlineExceeded) }

func (ctx *controllableDeadlineContext) finish(err error) {
	if err == nil {
		err = context.Canceled
	}
	ctx.once.Do(func() {
		ctx.err = err
		close(ctx.done)
	})
}
