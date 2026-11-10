// Package session owns TUI operations independently of the renderer. Cancelling
// observation never cancels an admission or another owner-controlled effect.
package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var ErrClosing = errors.New("the terminal session is closing")

// Kind separates cooperative reads from operations whose lifetime is owned by
// their existing application boundary, including write-capable observations.
type Kind uint8

const (
	Read Kind = iota
	Effect
)

// Reference is reconciliation guidance, not evidence that an operation failed.
type Reference struct {
	Description string
	Command     []string
}

type Operation struct {
	ID        uint64
	Kind      Kind
	Reference Reference
}

// Disposition carries only the lifecycle consequences needed after the renderer
// stops. The application remains the authority for the operation's actual result.
type Disposition struct {
	Err             error
	RestartRequired bool
}

type Completion struct {
	Operation
	Disposition
}

type State struct {
	Pending   []Operation
	Completed []Completion
}

// Tracker is the sole owner of session worker registration. Readers consume
// immutable snapshots; only Start and the worker's completion mutate entries.
type Tracker struct {
	mu       sync.Mutex
	reads    context.Context
	cancel   context.CancelFunc
	closing  bool
	next     uint64
	active   map[uint64]Operation
	finished map[uint64]Completion
	changed  chan struct{}
}

func New(parent context.Context) *Tracker {
	ctx, cancel := context.WithCancel(parent)
	return &Tracker{reads: ctx, cancel: cancel, active: make(map[uint64]Operation), finished: make(map[uint64]Completion), changed: make(chan struct{}, 1)}
}

// Start registers before launching, so rapid repeated submit/quit cannot miss
// an operation. Effect contexts deliberately inherit neither session cancellation
// nor a new deadline; operation owners preserve their existing deadlines.
func (t *Tracker) Start(kind Kind, ref Reference, work func(context.Context) error, complete func(uint64, error)) (uint64, error) {
	return t.start(kind, ref, func(ctx context.Context) Disposition { return Disposition{Err: work(ctx)} }, complete)
}

// StartEffect retains its completion until the renderer acknowledges it. A
// force-exit race cannot erase the reconciliation reference or cleanup warning.
func (t *Tracker) StartEffect(ref Reference, work func(context.Context) Disposition, complete func(uint64, error)) (uint64, error) {
	return t.start(Effect, ref, work, complete)
}

func (t *Tracker) start(kind Kind, ref Reference, work func(context.Context) Disposition, complete func(uint64, error)) (uint64, error) {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return 0, ErrClosing
	}
	t.next++
	id := t.next
	ref.Command = append([]string(nil), ref.Command...)
	t.active[id] = Operation{ID: id, Kind: kind, Reference: ref}
	ctx := t.reads
	if kind == Effect {
		ctx = context.WithoutCancel(t.reads)
	}
	t.mu.Unlock()
	go t.run(id, ctx, work, complete)
	return id, nil
}

func (t *Tracker) run(id uint64, ctx context.Context, work func(context.Context) Disposition, complete func(uint64, error)) {
	var result Disposition
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Err = fmt.Errorf("terminal operation panicked (%T)", recovered)
		}
		t.mu.Lock()
		operation := t.active[id]
		if operation.Kind == Effect {
			t.finished[id] = Completion{Operation: operation, Disposition: result}
		}
		delete(t.active, id)
		t.mu.Unlock()
		select {
		case t.changed <- struct{}{}:
		default:
		}
		if complete != nil {
			complete(id, result.Err)
		}
	}()
	result = work(ctx)
}

// Stop prevents further launches and cancels only cooperative reads. Completion
// is observable through Pending/Changed; the renderer remains responsive while
// effects settle. Force exit never calls another owner's cancellation path.
func (t *Tracker) Stop() {
	t.mu.Lock()
	t.closing = true
	t.mu.Unlock()
	t.cancel()
}

func (t *Tracker) Pending() []Operation {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make([]Operation, 0, len(t.active))
	for _, op := range t.active {
		op.Reference.Command = append([]string(nil), op.Reference.Command...)
		result = append(result, op)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Snapshot atomically observes the active-to-completed transition. Separate
// snapshots could miss a completion between their reads.
func (t *Tracker) Snapshot() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := State{Pending: make([]Operation, 0, len(t.active)), Completed: make([]Completion, 0, len(t.finished))}
	for _, operation := range t.active {
		operation.Reference.Command = append([]string(nil), operation.Reference.Command...)
		state.Pending = append(state.Pending, operation)
	}
	for _, completion := range t.finished {
		completion.Reference.Command = append([]string(nil), completion.Reference.Command...)
		state.Completed = append(state.Completed, completion)
	}
	sort.Slice(state.Pending, func(i, j int) bool { return state.Pending[i].ID < state.Pending[j].ID })
	sort.Slice(state.Completed, func(i, j int) bool { return state.Completed[i].ID < state.Completed[j].ID })
	return state
}

func (t *Tracker) Acknowledge(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.finished, id)
}

// JoinReads waits only for cooperative observations after Stop; it never adds a
// deadline or cancellation to an Effect owned by the application.
func (t *Tracker) JoinReads(ctx context.Context) error {
	for {
		reading := false
		for _, operation := range t.Pending() {
			reading = reading || operation.Kind == Read
		}
		if !reading {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.changed:
		}
	}
}

func (t *Tracker) Changed() <-chan struct{} { return t.changed }

// ReadContext permits a view to derive a shorter observation lifetime.
func (t *Tracker) ReadContext() context.Context { return t.reads }
