package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func closeFileQuietly(f *os.File) {
	if err := f.Close(); err != nil {
		return
	}
}

func platformBarrierDir(dirPath string) error {
	dir, err := os.Open(dirPath)
	if err != nil {
		return fmt.Errorf("opening dir for barrier %s: %w", dirPath, err)
	}
	defer closeFileQuietly(dir)
	return platformBarrierFile(dir)
}

func findDirectoriesToCreate(cleanTarget string) ([]string, error) {
	var toCreate []string
	cur := cleanTarget
	for {
		lInfo, statErr := os.Lstat(cur)
		if statErr == nil {
			if lInfo.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("ancestor %s is a symlink", cur)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("stat %s: %w", cur, statErr)
		}
		toCreate = append([]string{cur}, toCreate...)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return toCreate, nil
}

func barrierDirAndParent(dir string, injector FaultInjector) error {
	if injector != nil {
		if bErr := injector.OnDirBarrier(dir); bErr != nil {
			return fmt.Errorf("injected dir barrier error: %w", bErr)
		}
	}
	if err := platformBarrierDir(dir); err != nil {
		return fmt.Errorf("barrying newly created dir %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	if injector != nil {
		if pbErr := injector.OnParentDirBarrier(parent); pbErr != nil {
			return fmt.Errorf("injected parent dir barrier error: %w", pbErr)
		}
	}
	if err := platformBarrierDir(parent); err != nil {
		return fmt.Errorf("barrying parent dir %s: %w", parent, err)
	}
	return nil
}

func createDirectoriesWithAncestorBarrier(targetDir string, injector FaultInjector) error {
	cleanTarget := filepath.Clean(targetDir)
	info, err := os.Lstat(cleanTarget)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", cleanTarget)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", cleanTarget)
		}
		return barrierDirAndParent(cleanTarget, injector)
	}

	toCreate, fErr := findDirectoriesToCreate(cleanTarget)
	if fErr != nil {
		return fErr
	}

	// The first existing ancestor may have been created by an earlier failed
	// attempt. Repair its own parent entry before relying on new descendants.
	if len(toCreate) > 0 {
		if err := barrierDirAndParent(filepath.Dir(toCreate[0]), injector); err != nil {
			return err
		}
	}
	for _, dir := range toCreate {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
		if err := barrierDirAndParent(dir, injector); err != nil {
			return err
		}
	}
	return nil
}

func writeStageContent(f *os.File, data []byte, destPath string, injector FaultInjector) error {
	toWrite := data
	if injector != nil {
		n, wErr := injector.OnStageWrite(destPath, data)
		if wErr != nil {
			return fmt.Errorf("injected stage write error: %w", wErr)
		}
		if n < 0 || n > len(data) {
			return errors.New("invalid injected write count")
		}
		if n < len(data) {
			toWrite = data[:n]
		}
	}
	n, wErr := f.Write(toWrite)
	if wErr != nil {
		return fmt.Errorf("writing stage file: %w", wErr)
	}
	if n < len(data) {
		return errors.New("short write on stage file")
	}
	return nil
}

func barrierAndCloseStage(f *os.File, destPath string, injector FaultInjector) error {
	if injector != nil {
		if bErr := injector.OnStageBarrier(destPath); bErr != nil {
			return fmt.Errorf("injected stage barrier error: %w", bErr)
		}
	}
	if bErr := platformBarrierFile(f); bErr != nil {
		return fmt.Errorf("barrying stage file: %w", bErr)
	}

	if injector != nil {
		if cErr := injector.OnStageClose(destPath); cErr != nil {
			return fmt.Errorf("injected stage close error: %w", cErr)
		}
	}
	if cErr := f.Close(); cErr != nil {
		return fmt.Errorf("closing stage file: %w", cErr)
	}
	return nil
}

// stageAndCommit stages bytes using create-once publication. Callers enforce
// record-specific limits; control records use marshalControlRecord.
func (s *Store) stageAndCommit(dirPath, filename string, data []byte, injector FaultInjector) (bool, error, error) {
	return s.stageReader(dirPath, filename, bytes.NewReader(data), injector)
}

func (s *Store) stageReader(dirPath, filename string, reader io.Reader, injector FaultInjector) (committed bool, cleanupErr, resultErr error) {
	return s.stageReaderContext(context.Background(), dirPath, filename, reader, injector)
}

func (s *Store) stageReaderContext(ctx context.Context, dirPath, filename string, reader io.Reader, injector FaultInjector) (committed bool, cleanupErr, resultErr error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.maintLock.LockSH(ctx); err != nil {
		return false, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, s.maintLock.Unlock()) }()
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	if filename == "." || filename == ".." || filepath.Base(filename) != filename {
		return false, nil, errors.New("unsafe record basename")
	}
	dir, err := s.openDir(dirPath)
	if err != nil {
		return false, nil, err
	}
	defer closeFileQuietly(dir)
	id, err := task.NewRandomID()
	if err != nil {
		return false, nil, err
	}
	stage := filepath.Join(dirPath, "stage."+id+".tmp")
	dest := filepath.Join(dirPath, filename)
	f, err := s.openStageContext(ctx, stage, dest, injector)
	if err != nil {
		return false, nil, err
	}
	if err = writeStageReader(f, reader, dest, injector); err != nil {
		return false, nil, errors.Join(err, f.Close())
	}
	if err = barrierAndCloseStage(f, dest, injector); err != nil {
		return false, nil, errors.Join(err, f.Close())
	}
	committed, cleanupErr, resultErr = s.commitStage(stage, dest, injector)
	if !committed && errors.Is(resultErr, os.ErrExist) {
		// stageReader owns this newly created stage. A competing publisher
		// owns dest, so clean only this attempt's stage before returning the
		// publication race to the caller for winner verification.
		cleanupErr = errors.Join(cleanupErr, s.cleanupStage(stage, dest, injector))
	}
	return committed, cleanupErr, resultErr
}

// openStageContext observes cancellation at the staging boundary. Once
// creation starts, the caller retains ownership through writes and barriers.
func (s *Store) openStageContext(ctx context.Context, stage, dest string, injector FaultInjector) (*os.File, error) {
	if observer, ok := injector.(StageCreateInjector); ok {
		if err := observer.OnBeforeStageCreate(stage, dest); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.openFile(stage, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY)
}

func (s *Store) commitStage(stage, dest string, injector FaultInjector) (bool, error, error) {
	var err error
	dirPath := filepath.Dir(dest)
	if injector != nil {
		if err = injector.OnLink(stage, dest); err != nil {
			return false, nil, err
		}
	}
	if err = s.link(stage, dest); err != nil {
		return false, nil, err
	}
	if injector != nil {
		if err = injector.OnPostLinkDirBarrier(dest); err != nil {
			return false, nil, fmt.Errorf("%w: %w", task.ErrUncertainDurability, err)
		}
	}
	if err = s.barrierDir(dirPath); err != nil {
		return false, nil, fmt.Errorf("%w: %w", task.ErrUncertainDurability, err)
	}
	if injector != nil {
		if err = injector.OnAfterLinkDirBarrier(dest); err != nil {
			return true, err, nil
		}
	}
	return true, s.cleanupStage(stage, dest, injector), nil
}

func writeStageReader(f *os.File, r io.Reader, dest string, inj FaultInjector) error {
	buffer := make([]byte, 32*1024)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			if e := writeStageContent(f, buffer[:n], dest, inj); e != nil {
				return e
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// CleanupDestinationInjector receives the authoritative destination before
// cleanup hooks. A payload cleanup can never impersonate outcome cleanup.
type CleanupDestinationInjector interface {
	OnCleanupDestination(destination, stage string) error
}

func (s *Store) cleanupStage(stage, dest string, inj FaultInjector) error {
	if observer, ok := inj.(CleanupDestinationInjector); ok {
		if err := observer.OnCleanupDestination(dest, stage); err != nil {
			return err
		}
	}
	if inj != nil {
		if err := inj.OnCleanupUnlink(stage); err != nil {
			return err
		}
	}
	if err := s.unlink(stage); err != nil {
		return err
	}
	dir := filepath.Dir(stage)
	if inj != nil {
		if err := inj.OnCleanupDirBarrier(dir); err != nil {
			return err
		}
	}
	return s.barrierDir(dir)
}
