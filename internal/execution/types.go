// Package execution owns one provider invocation and its finite capture workers.
// It never submits work or signals a process. A caller supplies an already
// prepared task, a one-use start permit, and a compiled adapter's launch plan.
package execution

import (
	"context"
	"io"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Plan is constructed by a trusted compiled adapter, never from public raw argv.
type Plan struct {
	Executable  string
	Arguments   []string
	Directory   string
	Environment []string
	Predicate   task.PredicateRef
}

// IdentityObserver observes stdout without deciding publication. It must retain
// bounded parser state, continue accepting bytes after semantic faults, and only
// report validated identities through its runner-owned callback.
type IdentityObserver interface {
	Observe([]byte)
	Complete() error
}

// Stopper requests a single durable budget stop through the bound supervisor.
// Context only bounds observation: it must never cancel an external process.
// Implementations return when observation ends and retain any in-flight Wait.
type Stopper interface {
	RequestBudget(context.Context, time.Time) error
}

// Clock supplies monotonic in-memory launch timing. Wall times are evidence only.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

// Hooks are an explicit acceptance dependency, absent from production command
// construction. Callbacks must be finite and concurrency-safe. Start and Wait
// replace only the named owned operation, allowing definite failure and delayed
// operation tests; they never grant another permit or launch attempt.
type Hooks struct {
	Event   func(string)
	Start   func(*exec.Cmd) error
	Wait    func(*exec.Cmd) error
	Capture func(string, io.Writer, io.Reader) error
}

type Options struct {
	Clock    Clock
	Stopper  Stopper
	Identity IdentityObserver
	Hooks    Hooks
}

type Result struct {
	Outcome      *task.OutcomeRecord
	CleanupError error
	ReceiptError error
	StopError    error
	Error        error
}

func (o Options) clock() Clock {
	if o.Clock != nil {
		return o.Clock
	}
	return realClock{}
}

func (o Options) emit(name string) {
	if o.Hooks.Event != nil {
		o.Hooks.Event(name)
	}
}

func (o Options) start(cmd *exec.Cmd) error {
	if o.Hooks.Start != nil {
		return o.Hooks.Start(cmd)
	}
	return cmd.Start()
}

func (o Options) wait(cmd *exec.Cmd) error {
	if o.Hooks.Wait != nil {
		return o.Hooks.Wait(cmd)
	}
	return cmd.Wait()
}
