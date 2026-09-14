package taskdir

import (
	"errors"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type permitState struct {
	taskID   string
	consumed bool
}
type leaseState struct {
	mu            sync.Mutex
	lock          *LockFile
	maintenance   *LockFile
	released      bool
	releaseErr    error
	activeWriters int
	sealed        bool
	poisoned      error
	permit        *permitState
}

func (s *leaseState) release() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return s.releaseErr
	}
	if s.activeWriters != 0 {
		return task.ErrEvidenceFault
	}
	s.released = true
	s.releaseErr = errors.Join(s.lock.Unlock(), s.maintenance.Unlock())
	return s.releaseErr
}

func consume(state *permitState, lease *leaseState) error {
	if state == nil || lease == nil {
		return task.ErrInvalidPermit
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released {
		return task.ErrInvalidPermit
	}
	if state != lease.permit {
		return task.ErrInvalidPermit
	}
	if state.consumed {
		return task.ErrPermitAlreadyUsed
	}
	state.consumed = true
	return nil
}

func isConsumed(state *permitState, lease *leaseState) bool {
	if state == nil || lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return state.consumed
}

// AdmissionLease keeps both maintenance and admission ownership until Release.
// Copies share one private state, so they cannot duplicate authority.
type AdmissionLease struct{ state *leaseState }

func newAdmissionLease(lock, maint *LockFile) *AdmissionLease {
	return &AdmissionLease{state: &leaseState{lock: lock, maintenance: maint}}
}

func (l *AdmissionLease) Release() error {
	if l == nil {
		return nil
	}
	return l.state.release()
}

// RunnerLease retains run and maintenance ownership while capture is active.
type RunnerLease struct{ state *leaseState }

func newRunnerLease(run, maint *LockFile, _ string) *RunnerLease {
	return &RunnerLease{state: &leaseState{lock: run, maintenance: maint}}
}

func (l *RunnerLease) Release() error {
	if l == nil {
		return nil
	}
	return l.state.release()
}

func (l *RunnerLease) HasActiveWriters() bool {
	if l == nil || l.state == nil {
		return false
	}
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	return l.state.activeWriters > 0
}

type SubmissionPermit struct {
	record task.SubmitRecord
	state  *permitState
	lease  *AdmissionLease
}

func newSubmissionPermit(id string, lease *AdmissionLease, record task.SubmitRecord) *SubmissionPermit {
	state := &permitState{taskID: id}
	lease.state.permit = state
	return &SubmissionPermit{state: state, lease: lease, record: record}
}

func (p *SubmissionPermit) TaskID() string {
	if p == nil || p.state == nil {
		return ""
	}
	return p.state.taskID
}

func (p *SubmissionPermit) Consume() error {
	if p == nil || p.lease == nil {
		return task.ErrInvalidPermit
	}
	return consume(p.state, p.lease.state)
}

func (p *SubmissionPermit) Release() error {
	if p == nil {
		return nil
	}
	return p.lease.Release()
}

func (p *SubmissionPermit) IsConsumed() bool {
	if p == nil || p.lease == nil {
		return false
	}
	return isConsumed(p.state, p.lease.state)
}

type StartPermit struct {
	record task.ProviderStartRecord
	state  *permitState
	lease  *RunnerLease
}

func newStartPermit(id string, lease *RunnerLease, record task.ProviderStartRecord) *StartPermit {
	state := &permitState{taskID: id}
	lease.state.permit = state
	return &StartPermit{state: state, lease: lease, record: record}
}

func (p *StartPermit) TaskID() string {
	if p == nil || p.state == nil {
		return ""
	}
	return p.state.taskID
}

func (p *StartPermit) Consume() error {
	if p == nil || p.lease == nil {
		return task.ErrInvalidPermit
	}
	return consume(p.state, p.lease.state)
}

func (p *StartPermit) Release() error {
	if p == nil {
		return nil
	}
	return p.lease.Release()
}

func (p *StartPermit) RunnerLease() *RunnerLease {
	if p == nil {
		return nil
	}
	return p.lease
}

func (p *StartPermit) IsConsumed() bool {
	if p == nil || p.lease == nil {
		return false
	}
	return isConsumed(p.state, p.lease.state)
}

// Record returns a copy of the exact immutable authority record, never a writable pointer.
func (p *SubmissionPermit) Record() (*task.SubmitRecord, error) {
	if p == nil || p.lease == nil || p.state == nil {
		return nil, task.ErrInvalidPermit
	}
	p.lease.state.mu.Lock()
	defer p.lease.state.mu.Unlock()
	if p.lease.state.released || p.lease.state.permit != p.state {
		return nil, task.ErrInvalidPermit
	}
	record := p.record
	return &record, nil
}

func (p *StartPermit) Record() (*task.ProviderStartRecord, error) {
	if p == nil || p.lease == nil || p.state == nil {
		return nil, task.ErrInvalidPermit
	}
	p.lease.state.mu.Lock()
	defer p.lease.state.mu.Unlock()
	if p.lease.state.released || p.lease.state.permit != p.state {
		return nil, task.ErrInvalidPermit
	}
	record := p.record
	return &record, nil
}
