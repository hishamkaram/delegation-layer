// Package runtime owns the application loop for recurring schedules. SQLite
// owns claims and cursors; the task lifecycle owns execution and closure. This
// package composes those owners without launching providers or reading task files.
package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// ErrRootIdentity is a root-owner fault, unlike uncertainty confined to one
// task. Continuing against a different root would violate the held lease.
var ErrRootIdentity = errors.New("scheduler state root identity changed")

// ErrTaskCleanupUnresolved marks a task invocation whose real cleanup owners
// remain in its error graph. The runtime must stop admission and join workers.
var ErrTaskCleanupUnresolved = errors.New("scheduler task cleanup unresolved")

var ErrAttemptPersistence = errors.New("scheduler admission diagnostic could not be persisted")

const (
	// AdmissionLimit bounds unfinished admissions, including retained control work.
	AdmissionLimit = 4
	// ClaimWindow permits ordinary timer jitter without accumulating a backlog.
	ClaimWindow = 5 * time.Second
	// IdleInterval is a maximum idle wait, not a dispatch latency guarantee.
	IdleInterval = time.Second
)

// DispatchRequest is an immutable snapshot for one newly committed occurrence.
// Its task ID is already durable. Only that claim's caller may invoke Dispatch.
type DispatchRequest struct {
	TaskID     string
	Definition contract.Definition
}

// TaskState is an observation, never launch authority. Release is supplied only
// by the lifecycle owner after proving whole-record absence, canonical closure,
// or a permanent fenced-never-started disposition. The complete proof is checked
// there; the release's reference alone never proves closure.
type TaskState struct {
	Observation contract.Observation
	Release     *contract.Release
	Attempt     *contract.AttemptFailure
}

// Tasks is the loop's consumer-owned execution boundary. Once admitted by the
// loop, an invocation owns its lifetime and returns only after its retained
// supervisor-control work settles. Service cancellation stops new invocations;
// it neither interrupts an existing invocation nor cancels admitted tasks. Close
// joins those invocations without a shutdown-time guarantee. Observe reuses
// owner-driven Status recovery for the same task, never a replacement dispatch.
type Tasks interface {
	Dispatch(DispatchRequest) (TaskState, error)
	Observe(string) (TaskState, error)
}

// Clock supplies business wall time for calendar decisions and persisted facts.
// Wake cadence is independent of wall-clock changes and belongs to Waker.
type Clock interface {
	Now() time.Time
}

// Waker waits for a rescan hint, returning nil on a wake or ctx.Err() on
// cancellation. Implementations must settle before returning on cancellation;
// the loop cancels and joins every wait when a worker result arrives first.
// Wakes confer no claim or dispatch authority. The caller owns source lifecycle.
type Waker interface {
	Wait(context.Context) error
}

// Event contains bounded public identifiers/codes, never provider output or an
// inherited environment. The caller owns reporting; events do not own state.
type Event struct {
	ScheduleID string
	TaskID     string
	Phase      string
	Code       string
	Stage      string
	NextAction string
}

// Config supplies the existing owners. Nil Clock, Waker and NewTaskID select
// native wall time, an interruptible IdleInterval timer and the existing task ID
// generator. Report is optional. Store, Waker lifecycle and the exclusive
// scheduler owner lease belong to the caller.
type Config struct {
	Store             contract.Store
	Tasks             Tasks
	Clock             Clock
	Waker             Waker
	NewTaskID         func() (string, error)
	NewNotificationID func() (string, error)
	Report            func(Event)
	Evidence          ResultReader
	Hooks             HookRunner
}
