package infrastructure

import (
	"context"
	"os"
	"sync"
)

// daemonWaitOwner belongs to one resident's serving lifetime. Channel identity
// owns completion; PID is diagnostic and never process or shutdown authority.
// Only the canonical daemon starter supplies handles, closed after its sole Wait.
type daemonWaitOwner struct {
	mu                 sync.Mutex
	origin             Origin
	registrationClosed bool
	active             map[<-chan struct{}]int
}

func newDaemonWaitOwner(origin Origin) (*daemonWaitOwner, error) {
	if _, err := NewOrigin(origin.Root(), origin.RootID()); err != nil {
		return nil, err
	}
	return &daemonWaitOwner{origin: origin, active: make(map[<-chan struct{}]int)}, nil
}

func (w *daemonWaitOwner) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	if w == nil || pid <= 0 || done == nil {
		return os.ErrInvalid
	}
	if root != w.origin.Root() {
		return ErrIdentity
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.registrationClosed {
		return os.ErrClosed
	}
	w.pruneCompleted()
	if existing, found := w.active[done]; found && existing != pid {
		return ErrIdentity
	}
	select {
	case <-done:
		return nil
	default:
		w.active[done] = pid
		return nil
	}
}

// Serving policy must stop acceptance and drain handlers before calling this.
// This gate owns registration only; it neither closes a process nor cancels work.
func (w *daemonWaitOwner) closeRegistration() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.registrationClosed = true
	w.pruneCompleted()
}

// join is for Stop after canonical shutdown and independent native absence.
// Owner-only upgrade preserves live daemons and must not call it. No collection
// mutex is held while waiting; a timeout retains incomplete handles for retry.
func (w *daemonWaitOwner) join(ctx context.Context) error {
	if w == nil || ctx == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if !w.registrationClosed {
		w.mu.Unlock()
		return os.ErrInvalid
	}
	pending := make([]<-chan struct{}, 0, len(w.active))
	for done := range w.active {
		pending = append(pending, done)
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.pruneCompleted()
	}()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// The collection owner holds mu for pruning, registration and snapshots.
func (w *daemonWaitOwner) pruneCompleted() {
	for done := range w.active {
		select {
		case <-done:
			delete(w.active, done)
		default:
		}
	}
}
