package testsupport

import (
	"context"
	"sync"
	"time"
)

// controllableDeadlineContext lets a test place a command in a controlled
// pending state before delivering the same DeadlineExceeded signal that a real
// finite caller deadline would provide.
type ControllableDeadlineContext struct {
	parent   context.Context
	deadline time.Time
	done     chan struct{}

	once sync.Once
	err  error
}

func NewControllableDeadlineContext(parent context.Context, timeout time.Duration) (*ControllableDeadlineContext, func()) {
	deadline := time.Now().Add(timeout)
	ctx := &ControllableDeadlineContext{parent: parent, deadline: deadline, done: make(chan struct{})}
	timer := time.AfterFunc(timeout, func() { ctx.Expire() })
	stopParent := context.AfterFunc(parent, func() { ctx.finish(parent.Err()) })
	return ctx, func() {
		timer.Stop()
		stopParent()
		ctx.finish(context.Canceled)
	}
}

func (ctx *ControllableDeadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, true }

func (ctx *ControllableDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *ControllableDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return ctx.err
	default:
		return nil
	}
}

func (ctx *ControllableDeadlineContext) Value(key any) any { return ctx.parent.Value(key) }

func (ctx *ControllableDeadlineContext) Expire() { ctx.finish(context.DeadlineExceeded) }

func (ctx *ControllableDeadlineContext) finish(err error) {
	if err == nil {
		err = context.Canceled
	}
	ctx.once.Do(func() {
		ctx.err = err
		close(ctx.done)
	})
}
