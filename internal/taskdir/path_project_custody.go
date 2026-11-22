package taskdir

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PathProjectCustody borrows its Store and an immutable path approval. The
// caller must keep the Store open until supervised work and validation finish.
// Each directory selection is owned only for its synchronous start callback.
type PathProjectCustody struct {
	store   *Store
	binding task.ConnectorContext
	mu      sync.Mutex
	fault   error
}

func (s *Store) NewPathProjectCustody(ctx context.Context, binding task.ConnectorContext) (*PathProjectCustody, error) {
	if s == nil || !binding.Project.IsPath() {
		return nil, task.ErrIdentityMismatch
	}
	if err := task.ValidateConnectorBinding(&task.TaskRecord{CanonicalCwd: binding.Project.Path, ConnectorContext: &binding}); err != nil {
		return nil, err
	}
	custody := &PathProjectCustody{store: s, binding: *binding.Clone()}
	if err := custody.ValidateProject(ctx); err != nil {
		return nil, err
	}
	return custody, nil
}

func (p *PathProjectCustody) ValidateProject(ctx context.Context) error {
	if p == nil || p.store == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.store.requirePathConnectorEnabled(ctx); err != nil {
		return p.rememberFault(err)
	}
	root, err := p.store.RootTokenContext(ctx)
	if err == nil && root != p.binding.RootToken {
		err = task.ErrIdentityMismatch
	}
	return p.rememberFault(err)
}

func (p *PathProjectCustody) WithProjectDirectory(ctx context.Context, use func(*os.File, task.ProjectLaunchContext) error) (resultErr error) {
	if use == nil {
		return task.ErrInvalidPermit
	}
	if err := p.ValidateProject(ctx); err != nil {
		return err
	}
	selection, err := OpenPathProjectDirectory(ctx, p.binding)
	if err != nil {
		return p.rememberFault(err)
	}
	defer func() {
		closeErr := p.rememberFault(selection.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
		// A successful native Start must retain its sole Wait. A close fault is
		// sticky and surfaces at FinishCommand and final ValidateProject.
	}()
	launch := task.ProjectLaunchContext{Binding: *p.binding.Clone(), Observed: selection.selection.observedLeaf}
	return use(selection.File(), launch)
}

// No mutex is held across filesystem work or the callback. Custody failures
// remain terminal even if a later pathname or root observation is restored.
func (p *PathProjectCustody) rememberFault(err error) error {
	contextOnly := task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil && !contextOnly {
		p.fault = errors.Join(p.fault, err)
	}
	return errors.Join(p.fault, err)
}
