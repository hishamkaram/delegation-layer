package taskdir

import (
	"context"
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
	f, err := openLeafAt(dir, filepath.Base(path), path, flags)
	if err != nil {
		return nil, err
	}
	if err = validatePrivateFile(f, false); err != nil {
		closeFileQuietly(f)
		return nil, err
	}
	return f, nil
}

// openLeafAt opens one entry relative to an already pinned directory without
// following a symlink. The caller validates and owns the returned descriptor;
// the open error is returned unwrapped so the caller can classify it.
func openLeafAt(dir *os.File, name, path string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
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

func (s *Store) readRecordContext(ctx context.Context, path string, dst any) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(s.readRecord(path, dst), ctx.Err())
}

func (s *Store) readBytes(path string, limit int64) ([]byte, error) {
	f, err := s.openFile(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	b, readErr := task.ReadBounded(f, limit)
	return b, errors.Join(readErr, f.Close())
}

func (s *Store) readBytesContext(ctx context.Context, path string, limit int64) ([]byte, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := s.readBytes(path, limit)
	return b, errors.Join(err, ctx.Err())
}

// Digest scratch bounds. The descriptor's stat size only chooses a buffer in
// this range; it never decides or records content identity.
const (
	minDigestScratch = 512
	maxDigestScratch = 32 << 10
)

// readDigest hashes at most limit+1 bytes of a rooted private file without
// retaining them. Only the bytes actually read establish the returned identity.
func (s *Store) readDigest(path string, limit int64) (string, int64, error) {
	f, err := s.openFile(path, unix.O_RDONLY)
	if err != nil {
		return "", 0, err
	}
	digest, size, readErr := digestOpenFile(f, limit)
	return digest, size, errors.Join(readErr, f.Close())
}

func (s *Store) readDigestContext(ctx context.Context, path string, limit int64) (string, int64, error) {
	if ctx == nil {
		return "", 0, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	digest, size, err := s.readDigest(path, limit)
	return digest, size, errors.Join(err, ctx.Err())
}

func digestOpenFile(f *os.File, limit int64) (string, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	return task.DigestBounded(f, limit, make([]byte, digestScratchSize(info.Size())))
}

// digestScratchSize clamps a possibly stale size hint to the scratch bounds.
func digestScratchSize(hint int64) int {
	return int(min(max(hint, minDigestScratch), maxDigestScratch))
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
	return s.mkdirContext(context.Background(), path, inj)
}

func (s *Store) mkdirContext(ctx context.Context, path string, inj FaultInjector) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rel, err := s.relative(path)
	if err != nil {
		return err
	}
	current := s.Root
	if rel == "." {
		return s.barrierDir(current)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if err = ctx.Err(); err != nil {
			return err
		}
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
		if e = ctx.Err(); e != nil {
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
