package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// WorkspaceOperationIDs lists the sorted identities of existing workspace
// operation directories through the pinned root. Each name is only a
// candidate index: the operation's own request record remains authoritative,
// and a directory whose request is not yet committed is still listed. An
// absent operations parent lists nothing and creates nothing. Any entry that
// is not a canonical identity naming a private directory, including a
// symlink, is refused rather than omitted.
func (s *Store) WorkspaceOperationIDs(ctx context.Context) (ids []string, resultErr error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err = s.maintLock.LockSH(ctx); err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, s.maintLock.Unlock())
		if resultErr != nil {
			ids = nil
		}
	}()
	return s.scanWorkspaceOperationIDs(ctx)
}

func (s *Store) scanWorkspaceOperationIDs(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent := filepath.Join(s.Root, workspaceOperationsDirectory)
	dir, err := s.openDir(parent)
	if workspaceOperationsAbsent(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []string{}
	canceled := func() error { return ctx.Err() }
	// The shared scanner owns Close and joins it with every other fault.
	if err = scanDirectoryNames(canceled, dir, func(name string) error {
		if candidateErr := s.requireWorkspaceOperationCandidate(parent, name); candidateErr != nil {
			return candidateErr
		}
		ids = append(ids, name)
		return nil
	}); err != nil {
		return nil, err
	}
	slices.Sort(ids)
	return ids, nil
}

// requireWorkspaceOperationCandidate admits one parent entry only when its
// name is a canonical identity and it opens as a private directory without
// following a symlink. It never reads the operation's records.
func (s *Store) requireWorkspaceOperationCandidate(parent, name string) error {
	if err := task.ValidateTaskID(name); err != nil {
		return fmt.Errorf("%w: unexpected workspace operation entry %q: %w", task.ErrEvidenceFault, name, err)
	}
	dir, err := s.openDir(filepath.Join(parent, name))
	if err != nil {
		return fmt.Errorf("%w: workspace operation entry %q is not a private directory: %w", task.ErrEvidenceFault, name, err)
	}
	return dir.Close()
}

// workspaceOperationsAbsent admits only an error whose every cause is
// absence; a fault joined to absence remains a fault.
func workspaceOperationsAbsent(err error) bool {
	return task.AllErrorCauses(err, isAbsentEntry)
}

func isAbsentEntry(cause error) bool {
	return errors.Is(cause, os.ErrNotExist)
}
