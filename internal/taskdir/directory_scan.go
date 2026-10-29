package taskdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// directoryNames is the part of an opened directory a streaming scan uses.
// *os.File satisfies it; callers keep opening and missing-directory handling.
type directoryNames interface {
	Readdirnames(n int) ([]string, error)
	Close() error
}

// scanDirectoryNames streams a directory one name at a time to visit and
// closes it exactly once. Names returned with a read error are visited before
// that error is considered. A visitor, read, close or cancellation fault stops
// the scan and every cause is joined; only a read error whose every cause is
// io.EOF ends the scan cleanly. canceled reports the caller's cancellation
// before each read and after Close; a caller without a context passes
// notCanceled.
func scanDirectoryNames(canceled func() error, dir directoryNames, visit func(string) error) error {
	var err error
	for {
		if err = canceled(); err != nil {
			return errors.Join(err, dir.Close())
		}
		names, readErr := dir.Readdirnames(1)
		for _, name := range names {
			if err = visit(name); err != nil {
				break
			}
		}
		end := readErr != nil && task.AllErrorCauses(readErr, isEndOfDirectory)
		if readErr != nil && !end {
			err = errors.Join(err, readErr)
		}
		if err != nil || end {
			break
		}
	}
	return errors.Join(err, dir.Close(), canceled())
}

// privateDirectoryIDs lists the sorted canonical identities of the private
// directories directly under one root-level directory while holding the
// shared maintenance lease. kind names the entries in refusals. An absent
// parent lists nothing and creates nothing; an entry that is not a canonical
// identity naming a private directory is refused rather than omitted.
func (s *Store) privateDirectoryIDs(ctx context.Context, directory, kind string) (ids []string, resultErr error) {
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
	return s.scanPrivateDirectoryIDs(ctx, filepath.Join(s.Root, directory), kind)
}

func (s *Store) scanPrivateDirectoryIDs(ctx context.Context, parent, kind string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.openDir(parent)
	if directoryAbsent(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []string{}
	canceled := func() error { return ctx.Err() }
	// The shared scanner owns Close and joins it with every other fault.
	if err = scanDirectoryNames(canceled, dir, func(name string) error {
		if candidateErr := s.requirePrivateDirectoryCandidate(parent, name, kind); candidateErr != nil {
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

// requirePrivateDirectoryCandidate admits one parent entry only when its name
// is a canonical identity and it opens as a private directory without
// following a symlink. It never reads the entry's records.
func (s *Store) requirePrivateDirectoryCandidate(parent, name, kind string) error {
	if err := task.ValidateTaskID(name); err != nil {
		return fmt.Errorf("%w: unexpected %s entry %q: %w", task.ErrEvidenceFault, kind, name, err)
	}
	dir, err := s.openDir(filepath.Join(parent, name))
	if err != nil {
		return fmt.Errorf("%w: %s entry %q is not a private directory: %w", task.ErrEvidenceFault, kind, name, err)
	}
	return dir.Close()
}

// directoryAbsent admits only an error whose every cause is absence; a fault
// joined to absence remains a fault.
func directoryAbsent(err error) bool {
	return task.AllErrorCauses(err, isAbsentEntry)
}

func isAbsentEntry(cause error) bool {
	return errors.Is(cause, os.ErrNotExist)
}

func notCanceled() error {
	return nil
}

func isEndOfDirectory(cause error) bool {
	return errors.Is(cause, io.EOF)
}
