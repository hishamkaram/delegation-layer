// Package pueue binds a compatible supervisor CLI to immutable task identity.
// Observation deadlines never cancel an external process or grant a retry.
package pueue

import (
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// FixtureVersion names the serialized test fixture baseline. Runtime
	// admission accepts any nonempty observed version whose behavior matches
	// the validated command/schema contract.
	FixtureVersion            = "4.0.4"
	DefaultObservationTimeout = 5 * time.Second
	MaxControlBytes           = 1 << 20
)

var (
	ErrConfiguration = errors.New("invalid pueue configuration")
	ErrBinding       = errors.New("pueue binding changed or unsupported")
	ErrUnknown       = errors.New("pueue observation is unknown")
	ErrInFlight      = errors.New("pueue command remains in flight")
	ErrControlLimit  = errors.New("pueue control output limit exceeded")
)

// Options controls caller observation and an optional explicit process context.
// Nil Resolution and Environment use current process control values at each
// fresh call; the default child environment is bounded to pueue's control
// variables. A nonnil Environment is passed through as supplied by the caller.
type Options struct {
	ObservationTimeout time.Duration
	Resolution         *ResolutionContext
	Environment        []string
	// Observer is an optional finite, concurrency-safe acceptance recorder.
	// It receives copied values and must not block process ownership.
	Observer func(CommandEvent)
}

// CommandEvent reports entry, successful Start, or actual completion. Entry
// does not establish Start success; a completed negative exit remains unknown.
type CommandEvent struct {
	CommandID string   `json:"command_id"`
	Stage     string   `json:"stage"`
	Argv      []string `json:"argv"`
	PID       int      `json:"pid"`
	ExitCode  int      `json:"exit_code"`
}

// Identity supplies immutable task identity and an optional already-bound job ID.
type Identity struct {
	RootID        string
	TaskID        string
	SpecSHA256    string
	MetaSHA256    string
	NumericTaskID *int64
}

func (i Identity) Label() string { return "delegate:" + i.RootID + ":" + i.TaskID }

// Launch transports identity only. The brief never appears in supervisor argv.
type Launch struct {
	RunnerExecutable string
	RootPath         string
}

type State string

const (
	StateUnknown State = "unknown"
	StateQueued  State = "queued"
	StateRunning State = "running"
	StateEnded   State = "ended"
)

// Observation is positive only when exactly one row matches the complete binding.
// Ended describes the supervised job, never all descendants or a task outcome.
type Observation struct {
	Matched       bool
	State         State
	Identity      Identity
	Binding       task.SupervisorRef
	NumericTaskID int64
	Pending       *Pending
}

// StopResult keeps request, routing, acknowledgment and observation separate.
// Acknowledged is nil when there is no completed response.
type StopResult struct {
	Requested     bool
	Matched       bool
	Attempted     bool
	Action        string
	NumericTaskID *int64
	Acknowledged  *bool
	InFlight      bool
	ObservedState State
	Message       string
	Pending       *Pending
}

// CommandResult is accessible only after natural Start/Wait ownership completes.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Started  bool
	PID      int
	Err      error
}

// Pending retains completion ownership after the caller's observation expires.
type Pending struct {
	done   chan struct{}
	result CommandResult
}

func (p *Pending) Done() <-chan struct{} { return p.done }

// Result returns copied output only after synchronization with natural Wait.
func (p *Pending) Result() (CommandResult, bool) {
	select {
	case <-p.done:
		r := p.result
		r.Stdout = append([]byte(nil), r.Stdout...)
		r.Stderr = append([]byte(nil), r.Stderr...)
		return r, true
	default:
		return CommandResult{}, false
	}
}
