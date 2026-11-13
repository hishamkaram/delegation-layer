package taskdir

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// WithProjectDirectory borrows the runner-owned directory for one callback.
// The callback receives no ownership; it must neither close nor retain the file.
// No lease mutex is held across filesystem checks, callback code or child I/O.
func (p *StartPermit) WithProjectDirectory(ctx context.Context, use func(*os.File, task.DirectoryIdentity) error) error {
	if p == nil || p.lease == nil || p.state == nil || use == nil {
		return task.ErrInvalidPermit
	}
	state := p.lease.state
	project, err := state.borrowProject(ctx, p.state, true)
	if err != nil {
		return err
	}
	defer state.finishProjectBorrow()
	if err = state.validateBorrowedProject(ctx, project); err != nil {
		return err
	}
	return use(project.file, project.binding.Project)
}

// ValidateProject latches observed directory or binding failures in the shared
// runner state. A later pathname restoration cannot authorize sealing again.
// Ordinary, unguarded tasks have no project custody and need no such check.
func (l *RunnerLease) ValidateProject(ctx context.Context) error {
	if l == nil || l.state == nil {
		return task.ErrInvalidPermit
	}
	project, err := l.state.borrowProject(ctx, nil, false)
	if err != nil || project == nil {
		return err
	}
	defer l.state.finishProjectBorrow()
	return l.state.validateBorrowedProject(ctx, project)
}

func (s *leaseState) borrowProject(ctx context.Context, permit *permitState, required bool) (*ConnectorContextLease, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, task.ErrInvalidPermit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released || s.sealed || (permit != nil && permit != s.permit) {
		return nil, task.ErrInvalidPermit
	}
	if s.poisoned != nil {
		return nil, s.poisoned
	}
	if s.project == nil {
		if required {
			return nil, task.ErrInvalidPermit
		}
		return nil, nil
	}
	s.projectBorrows++
	return s.project, nil
}

func (s *leaseState) finishProjectBorrow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectBorrows--
}

func (s *leaseState) validateBorrowedProject(ctx context.Context, project *ConnectorContextLease) error {
	err := project.Validate(ctx)
	contextOnly := task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil && !contextOnly {
		s.poisoned = errors.Join(s.poisoned, err)
	}
	return errors.Join(s.poisoned, err)
}
