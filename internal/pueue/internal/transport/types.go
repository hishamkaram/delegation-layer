package transport

import (
	"errors"
	"slices"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	ErrInFlight     = errors.New("pueue command remains in flight")
	ErrControlLimit = protocol.ErrControlLimit
)

const MaxControlBytes = protocol.MaxControlBytes

type (
	CommandEvent  = protocol.CommandEvent
	QueueSnapshot = protocol.QueueSnapshot
)

type CommandResult struct {
	snapshot *QueueSnapshot
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Started  bool
	PID      int
	Err      error
}

func (r CommandResult) Snapshot() (QueueSnapshot, bool) {
	if r.snapshot == nil {
		return QueueSnapshot{}, false
	}
	return protocol.CloneQueueSnapshot(*r.snapshot), true
}

func (r *CommandResult) SetSnapshot(snapshot QueueSnapshot) {
	if r != nil {
		copy := protocol.CloneQueueSnapshot(snapshot)
		r.snapshot = &copy
	}
}

type pendingStatusIdentity struct {
	binding task.SupervisorRef
	version string
}

type Pending struct {
	done     chan struct{}
	result   CommandResult
	args     []string
	status   *pendingStatusIdentity
	complete sync.Once
}

func NewPending(args []string) *Pending {
	return &Pending{done: make(chan struct{}), args: slices.Clone(args)}
}

func NewCompletedPending(args []string, result CommandResult) *Pending {
	pending := NewPending(args)
	CompletePending(pending, result)
	return pending
}

// CompletePending publishes the owning command's completed result. Started
// commands must have finished their exact Wait before this call.
func CompletePending(p *Pending, result CommandResult) {
	if p == nil {
		return
	}
	p.complete.Do(func() {
		p.result = cloneCommandResult(result)
		close(p.done)
	})
}

func (p *Pending) Done() <-chan struct{} { return p.done }

func (p *Pending) Args() []string {
	if p == nil {
		return nil
	}
	return slices.Clone(p.args)
}

func (p *Pending) VersionProbe() bool {
	return p != nil && len(p.args) > 0 && p.args[len(p.args)-1] == "--version"
}

// SetPendingStatusIdentity binds the status command before its handle is exposed.
func SetPendingStatusIdentity(p *Pending, binding task.SupervisorRef, version string) {
	if p == nil {
		return
	}
	p.status = &pendingStatusIdentity{binding: binding, version: version}
}

func (p *Pending) StatusIdentity() (task.SupervisorRef, string, bool) {
	if p == nil || p.status == nil {
		return task.SupervisorRef{}, "", false
	}
	return p.status.binding, p.status.version, true
}

func (p *Pending) MatchesArgs(want []string) bool {
	return p != nil && slices.Equal(p.args, want)
}

// Result returns copied output only after synchronization with natural Wait.
func (p *Pending) Result() (CommandResult, bool) {
	if p == nil {
		return CommandResult{}, false
	}
	select {
	case <-p.done:
		return cloneCommandResult(p.result), true
	default:
		return CommandResult{}, false
	}
}

func cloneCommandResult(result CommandResult) CommandResult {
	result.Stdout = slices.Clone(result.Stdout)
	result.Stderr = slices.Clone(result.Stderr)
	if snapshot, ok := result.Snapshot(); ok {
		result.SetSnapshot(snapshot)
	}
	return result
}

type InFlightError struct{ Pending *Pending }

func (e *InFlightError) Error() string { return ErrInFlight.Error() }
func (e *InFlightError) Unwrap() error { return ErrInFlight }
