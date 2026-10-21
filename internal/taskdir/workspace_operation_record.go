package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// ReadWorkspaceOperationRecord observes one named record of an existing
// workspace operation directory through the pinned root. It never opens or
// creates the operation lock and never writes. present=false with a nil error
// means only that the named entry is absent from the successfully opened and
// validated operation directory; an empty file is present. Every other
// outcome is an error with no data, including a missing operations parent or
// operation directory, an unsafe or oversized entry, cancellation, and any
// descriptor close or maintenance release fault. The record-family owner
// supplies the name and interprets the bytes. Each call is one observation,
// not an atomic inventory of the operation.
func (s *Store) ReadWorkspaceOperationRecord(ctx context.Context, operationID, name string) (data []byte, present bool, err error) {
	return s.readWorkspaceOperationRecord(ctx, operationID, name, closeOwnedFile)
}

// fileCloser releases one descriptor owned by a record observation. It is a
// parameter only so a test can prove that a close fault is never absence.
type fileCloser func(*os.File) error

func closeOwnedFile(f *os.File) error { return f.Close() }

func (s *Store) readWorkspaceOperationRecord(ctx context.Context, operationID, name string, closeFile fileCloser) (data []byte, present bool, err error) {
	if err = task.ValidateTaskID(operationID); err != nil {
		return nil, false, err
	}
	if err = validateControlName(name); err != nil {
		return nil, false, err
	}
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, false, err
	}
	defer cancel()
	if err = s.maintLock.LockSH(ctx); err != nil {
		return nil, false, err
	}
	defer func() {
		// A release or lifetime fault invalidates the observation, absence included.
		err = errors.Join(err, s.maintLock.Unlock(), ctx.Err())
		if err != nil {
			data, present = nil, false
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	directory, err := s.openDir(filepath.Join(s.Root, workspaceOperationsDirectory, operationID))
	if err != nil {
		return nil, false, fmt.Errorf("opening workspace operation %s: %w", operationID, err)
	}
	data, present, err = readPinnedRecord(directory, name, closeFile)
	return data, present, errors.Join(err, closeFile(directory))
}

// readPinnedRecord opens name relative to the pinned directory and classifies
// absence only at that leaf open. A present entry must be a private regular
// file within the control-record bound.
func readPinnedRecord(directory *os.File, name string, closeFile fileCloser) ([]byte, bool, error) {
	leaf, err := openLeafAt(directory, name, filepath.Join(directory.Name(), name), unix.O_RDONLY)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("opening workspace operation record %s: %w", name, err)
	}
	if err = validatePrivateFile(leaf, false); err != nil {
		return nil, false, errors.Join(fmt.Errorf("validating workspace operation record %s: %w", name, err), closeFile(leaf))
	}
	data, err := task.ReadControlRecord(leaf)
	if err = errors.Join(err, closeFile(leaf)); err != nil {
		return nil, false, fmt.Errorf("reading workspace operation record %s: %w", name, err)
	}
	return data, true, nil
}
