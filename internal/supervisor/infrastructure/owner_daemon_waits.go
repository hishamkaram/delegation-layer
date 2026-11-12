package infrastructure

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

var ErrDaemonWaitCustody = errors.New("resident does not own the selected daemon wait")

type daemonWaitBirth struct {
	pid   int
	birth func() (lifetime.ProcessRef, bool, error)
}

type daemonWaitCandidate struct {
	done         <-chan struct{}
	registration daemonWaitBirth
}

// daemonWaitOwner belongs to one resident's serving lifetime. Channel identity
// owns completion; PID is diagnostic and never process or shutdown authority.
// Only the canonical daemon starter supplies handles, closed after its sole Wait.
type daemonWaitOwner struct {
	mu                 sync.Mutex
	origin             Origin
	resident           *ownedResidentEntry
	registrationClosed bool
	active             map[<-chan struct{}]daemonWaitBirth
}

func newDaemonWaitOwner(origin Origin) (*daemonWaitOwner, error) {
	if _, err := NewOrigin(origin.Root(), origin.RootID()); err != nil {
		return nil, err
	}
	return &daemonWaitOwner{origin: origin, active: make(map[<-chan struct{}]daemonWaitBirth)}, nil
}

func (w *daemonWaitOwner) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	return w.registerDaemonWait(root, pid, done, nil)
}

// RegisterDaemonWaitBirth receives the canonical starter's immutable birth
// result. The getter is nonblocking and never observes a PID after its Wait.
// It is retained with the same actual completion handle, not a second owner.
func (w *daemonWaitOwner) RegisterDaemonWaitBirth(root string, pid int, done <-chan struct{}, birth func() (lifetime.ProcessRef, bool, error)) error {
	if birth == nil {
		return os.ErrInvalid
	}
	return w.registerDaemonWait(root, pid, done, birth)
}

func (w *daemonWaitOwner) registerDaemonWait(root string, pid int, done <-chan struct{}, birth func() (lifetime.ProcessRef, bool, error)) error {
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
	existing, found := w.active[done]
	if found {
		if existing.pid != pid || existing.birth != nil && birth != nil {
			return ErrIdentity
		}
		if existing.birth != nil {
			birth = existing.birth // Legacy duplicate registration cannot erase proof.
		}
	}
	select {
	case <-done:
		return nil
	default:
		w.active[done] = daemonWaitBirth{pid: pid, birth: birth}
		return nil
	}
}

// RequireDaemonWaitCustody observes only this currently serving resident's
// registered exact birth. It grants no mutation; the generation guard must
// independently revalidate authority at the actual restart commit.
func (w *daemonWaitOwner) RequireDaemonWaitCustody(ctx context.Context, root string, expected lifetime.ProcessRef) error {
	if w == nil || ctx == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if root != w.origin.Root() {
		return ErrIdentity
	}
	if _, err := lifetime.NewProcessRef(expected.PID(), expected.StartToken()); err != nil {
		return err
	}
	if err := w.requireServing(ctx); err != nil {
		return err
	}
	candidates, err := w.birthCandidates(expected.PID())
	if err != nil {
		return err
	}
	if err = matchDaemonWaitBirth(expected, candidates); err != nil {
		return err
	}
	if err = w.requireRegistrationOpen(); err != nil {
		return err
	}
	return w.requireServing(ctx)
}

func (w *daemonWaitOwner) requireRegistrationOpen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.registrationClosed {
		return os.ErrClosed
	}
	return nil
}

func (w *daemonWaitOwner) hasActiveWait() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneCompleted()
	return len(w.active) != 0
}

func (w *daemonWaitOwner) requireServing(ctx context.Context) error {
	entry := w.resident
	if entry == nil {
		return ErrDaemonWaitCustody
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed || entry.grant == nil || entry.grant.entry != entry || entry.endpoint == nil || entry.journal == nil || entry.lease == nil {
		return os.ErrClosed
	}
	inputs := entry.operation.Intent().Inputs
	if inputs.Root != w.origin.Root() || inputs.RootID != w.origin.RootID() {
		return ErrIdentity
	}
	return entry.validateResident(ctx)
}

func (w *daemonWaitOwner) birthCandidates(pid int) ([]daemonWaitCandidate, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.registrationClosed {
		return nil, os.ErrClosed
	}
	w.pruneCompleted()
	var candidates []daemonWaitCandidate
	for done, registration := range w.active {
		if registration.pid == pid && registration.birth != nil {
			candidates = append(candidates, daemonWaitCandidate{done: done, registration: registration})
		}
	}
	return candidates, nil
}

// The immutable getter runs outside the registry and entry mutexes. Only a
// matching exact birth with its actual Wait still open can satisfy this read.
func matchDaemonWaitBirth(expected lifetime.ProcessRef, candidates []daemonWaitCandidate) error {
	var failure error
	pending := false
	for _, candidate := range candidates {
		observed, ready, err := candidate.registration.birth()
		if err != nil {
			failure = errors.Join(failure, err)
			continue
		}
		if !ready {
			pending = true
			continue
		}
		same, compareErr := lifetime.SameProcess(observed, expected)
		if compareErr != nil {
			failure = errors.Join(failure, compareErr)
			continue
		}
		if same {
			select {
			case <-candidate.done:
			default:
				return nil
			}
		}
	}
	if failure != nil {
		return errors.Join(ErrDaemonWaitCustody, failure)
	}
	if pending {
		return ErrOwnerBusy
	}
	return ErrDaemonWaitCustody
}

// The runtime must drain callbacks and join retained preparation workers first.
// This gate owns registration only; it neither closes a process nor cancels work.
func (w *daemonWaitOwner) closeRegistration() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.registrationClosed = true
	w.pruneCompleted()
}

// pendingAfterDrain reads this resident's actual completion handles. Only the
// runtime closes registration; an open registration grants no release.
func (w *daemonWaitOwner) pendingAfterDrain() (bool, error) {
	if w == nil {
		return false, os.ErrInvalid
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.registrationClosed {
		return false, os.ErrInvalid
	}
	w.pruneCompleted()
	return len(w.active) != 0, nil
}

// join retains the resident until its actual daemon command owners complete.
// It never shuts down a daemon. No collection mutex is held while waiting; a
// canceled observation retains incomplete handles and grants no closure.
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
