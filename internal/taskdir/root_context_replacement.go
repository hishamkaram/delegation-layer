package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"golang.org/x/sys/unix"
)

// RootContextReplacementInjector exercises the actual rooted replacement and
// readback boundary. It supplies faults, never a replacement storage owner.
type RootContextReplacementInjector interface {
	OnBeforeRootContextRename(stage, destination string) error
	OnAfterRootContextRename(destination string) error
	OnRootContextBarrier(path string) error
}

func (s *Store) replaceRootContextRecord(ctx context.Context, expectedToken string, old rootContextSnapshot, candidate []byte) error {
	id, err := randomid.NewRandomID()
	if err != nil {
		return err
	}
	name := "root-context." + id + ".tmp"
	created, err := s.stageRootContextCandidate(ctx, name, candidate)
	if err != nil {
		readbackErr := s.classifyRootContextReplacement(context.WithoutCancel(ctx), old, candidate, err)
		if !created {
			return readbackErr
		}
		return errors.Join(readbackErr, s.removeRootContextCandidate(name))
	}
	// Recheck the exact observed root and history before rename. Our published
	// candidate is intentionally excluded from this check's stage classification.
	if err = s.revalidateRootContextReplacement(ctx, expectedToken, old, name); err != nil {
		return errors.Join(err, s.removeRootContextCandidate(name))
	}
	err = s.renameRootContextRecord(name)
	readbackErr := s.classifyRootContextReplacement(context.WithoutCancel(ctx), old, candidate, err)
	// Remove only this invocation's known stage. A missing name after rename
	// is expected; the authoritative root winner is never unlinked.
	return errors.Join(readbackErr, s.removeRootContextCandidate(name))
}

// Reuse the rooted canonical stage writer and file barrier, then replace in one
// rename. The exact stage name remains owned across every ambiguous failure.
func (s *Store) stageRootContextCandidate(ctx context.Context, name string, data []byte) (bool, error) {
	destination := filepath.Join(s.Root, "root.json")
	file, err := s.openStageContext(ctx, filepath.Join(s.Root, name), destination, s.faultInjector)
	if err != nil {
		return false, err
	}
	if err = writeStageReader(file, bytes.NewReader(data), destination, s.faultInjector); err != nil {
		return true, errors.Join(err, file.Close())
	}
	if err = barrierAndCloseStage(file, destination, s.faultInjector); err != nil {
		return true, errors.Join(err, file.Close())
	}
	return true, nil
}

func (s *Store) revalidateRootContextReplacement(ctx context.Context, expectedToken string, old rootContextSnapshot, ownedStage string) error {
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return err
	}
	_, data, err := s.readRootContextRecord(ctx)
	if err != nil {
		return err
	}
	if !pin.Equal(old.pin) || !bytes.Equal(data, old.data) {
		return task.ErrRequestConflict
	}
	if err = s.refuseRootContextStagesExcept(ctx, s.Root, ownedStage); err != nil {
		return err
	}
	history, _, _, err := s.rootContextHistory(ctx, expectedToken)
	if err != nil {
		return err
	}
	if history != old.result.HistorySHA256 {
		return task.ErrRequestConflict
	}
	return ctx.Err()
}

func (s *Store) renameRootContextRecord(name string) (resultErr error) {
	destination := filepath.Join(s.Root, "root.json")
	hooks, hooked := s.faultInjector.(RootContextReplacementInjector)
	if hooked {
		if err := hooks.OnBeforeRootContextRename(filepath.Join(s.Root, name), destination); err != nil {
			return err
		}
	}
	parent, err := s.openDir(s.Root)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, parent.Close()) }()
	if err = unix.Renameat(int(parent.Fd()), name, int(parent.Fd()), "root.json"); err != nil {
		return err
	}
	if hooked {
		return hooks.OnAfterRootContextRename(destination)
	}
	return nil
}

// Replacement readback has four outcomes: exact old, exact candidate, a
// conflicting winner, or absent/unreadable. Absence never initializes a root.
func (s *Store) classifyRootContextReplacement(ctx context.Context, old rootContextSnapshot, candidate []byte, cause error) error {
	winner, err := s.readBytesContext(ctx, filepath.Join(s.Root, "root.json"), task.MaxControlRecordSize)
	if err == nil {
		_, err = decodeRootContextRecord(winner)
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("%w: root replacement unreadable: %w", task.ErrUncertainDurability, err))
	}
	if bytes.Equal(winner, candidate) {
		pin, pinErr := s.PinRootContext(ctx)
		if pinErr != nil || !pin.Equal(old.pin) {
			return errors.Join(cause, pinErr, task.ErrIdentityMismatch)
		}
		if err = s.barrierRootContextRecord(candidate); err != nil {
			return errors.Join(cause, fmt.Errorf("%w: root replacement durability: %w", task.ErrUncertainDurability, err))
		}
		return nil
	}
	if bytes.Equal(winner, old.data) {
		return errors.Join(cause, fmt.Errorf("%w: root context replacement did not commit", task.ErrUncertainDurability))
	}
	return errors.Join(cause, fmt.Errorf("%w: root replacement has a conflicting winner", task.ErrInvariantFault))
}

func (s *Store) barrierRootContextRecord(expected []byte) (resultErr error) {
	path := filepath.Join(s.Root, "root.json")
	if hooks, ok := s.faultInjector.(RootContextReplacementInjector); ok {
		if err := hooks.OnRootContextBarrier(path); err != nil {
			return err
		}
	}
	file, err := s.openFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	data, err := task.ReadControlRecord(file)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, expected) {
		return task.ErrInvariantFault
	}
	return errors.Join(platformBarrierFile(file), s.barrierDir(s.Root))
}

func (s *Store) removeRootContextCandidate(name string) error {
	path := filepath.Join(s.Root, name)
	file, err := s.openFile(path, os.O_RDONLY)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return s.cleanupStage(path, filepath.Join(s.Root, "root.json"), s.faultInjector)
}
