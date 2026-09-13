package taskdir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// Every operation below begins at the pinned root descriptor. Opening each
// component with O_NOFOLLOW prevents a replaced parent from redirecting I/O.
func (s *Store) relative(path string) (string, error) {
	rel, err := filepath.Rel(s.Root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("path escapes state root: %s", path)
	}
	return rel, nil
}

func validatePrivateFile(f *os.File, directory bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	expectedMode := os.FileMode(0o600)
	if directory {
		expectedMode = 0o700
	}
	if st.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != expectedMode || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%w: unsafe owner or mode on %s", task.ErrEvidenceFault, f.Name())
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: unexpected file type on %s", task.ErrEvidenceFault, f.Name())
	}
	return nil
}

func (s *Store) openDir(path string) (*os.File, error) {
	rel, err := s.relative(path)
	if err != nil {
		return nil, err
	}
	current, err := s.rootHandle.Open(".")
	if err != nil {
		return nil, err
	}
	if err = validatePrivateFile(current, true); err != nil {
		closeFileQuietly(current)
		return nil, err
	}
	if rel == "." {
		return current, nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		fd, e := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeFileQuietly(current)
		if e != nil {
			return nil, e
		}
		current = os.NewFile(uintptr(fd), path)
		if e = validatePrivateFile(current, true); e != nil {
			closeFileQuietly(current)
			return nil, e
		}
	}
	return current, nil
}

func (s *Store) openFile(path string, flags int) (*os.File, error) {
	dir, err := s.openDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer closeFileQuietly(dir)
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = validatePrivateFile(f, false); err != nil {
		closeFileQuietly(f)
		return nil, err
	}
	return f, nil
}

func (s *Store) exists(path string) (bool, error) {
	f, err := s.openFile(path, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

func (s *Store) readRecord(path string, dst any) error {
	f, err := s.openFile(path, unix.O_RDONLY)
	if err != nil {
		return err
	}
	b, readErr := task.ReadControlRecord(f)
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return err
	}
	return task.DecodeStrict(b, dst)
}

func (s *Store) readBytes(path string, limit int64) ([]byte, error) {
	f, err := s.openFile(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	b, readErr := task.ReadBounded(f, limit)
	return b, errors.Join(readErr, f.Close())
}

func (s *Store) barrierDir(path string) error {
	f, err := s.openDir(path)
	if err != nil {
		return err
	}
	return errors.Join(platformBarrierFile(f), f.Close())
}

func (s *Store) unlink(path string) error {
	dir, err := s.openDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer closeFileQuietly(dir)
	return unix.Unlinkat(int(dir.Fd()), filepath.Base(path), 0)
}

func (s *Store) link(from, to string) error {
	parent, err := s.openDir(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer closeFileQuietly(parent)
	dest, err := s.openDir(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer closeFileQuietly(dest)
	return unix.Linkat(int(parent.Fd()), filepath.Base(from), int(dest.Fd()), filepath.Base(to), 0)
}

// Retry flushes every directory in the state-root chain, including preexisting
// entries: their existence cannot establish that an earlier parent sync succeeded.
func (s *Store) mkdir(path string, inj FaultInjector) error {
	rel, err := s.relative(path)
	if err != nil {
		return err
	}
	current := s.Root
	if rel == "." {
		return s.barrierDir(current)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		parent, e := s.openDir(current)
		if e != nil {
			return e
		}
		e = unix.Mkdirat(int(parent.Fd()), part, 0o700)
		closeFileQuietly(parent)
		if e != nil && !errors.Is(e, unix.EEXIST) {
			return e
		}
		next := filepath.Join(current, part)
		if e = s.barrierDirectoryEntry(next, current, inj); e != nil {
			return e
		}
		current = next
	}
	return nil
}

func copyChecked(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}

func (s *Store) barrierDirectoryEntry(child, parent string, inj FaultInjector) error {
	if inj != nil {
		if err := inj.OnDirBarrier(child); err != nil {
			return err
		}
	}
	if err := s.barrierDir(child); err != nil {
		return err
	}
	if inj != nil {
		if err := inj.OnParentDirBarrier(parent); err != nil {
			return err
		}
	}
	return s.barrierDir(parent)
}

// entryExists observes the name without opening or following its target. It is
// used only for presence diagnostics; authority always uses validated openFile.
func (s *Store) entryExists(path string) (bool, error) {
	dir, err := s.openDir(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer closeFileQuietly(dir)
	var st unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), filepath.Base(path), &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
