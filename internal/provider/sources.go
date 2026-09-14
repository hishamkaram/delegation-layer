package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// SourceBytes is transient resolver input. Only after its schema and policy
// have been validated may its non-secret digest enter an effective snapshot.
// No source contents are persisted by the reader.
type SourceBytes struct {
	Path    string
	Present bool
	Data    []byte
}

func ReadPolicySource(path string) (SourceBytes, error) {
	result := SourceBytes{Path: path}
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path || filepath.Clean(path) != path {
		return result, fmt.Errorf("%w: policy source must have a canonical path: %s", ErrProfileUnavailable, path)
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("%w: cannot inspect policy source %s: %w", ErrProfileUnavailable, path, err)
	}
	if !before.Mode().IsRegular() {
		return result, fmt.Errorf("%w: policy source is not a regular file: %s", ErrProfileUnavailable, path)
	}
	data, err := readStablePolicyFile(path, before)
	if err != nil {
		return result, fmt.Errorf("%w: cannot read stable policy source %s: %w", ErrProfileUnavailable, path, err)
	}
	result.Present, result.Data = true, data
	return result, nil
}

func readStablePolicyFile(path string, before os.FileInfo) (data []byte, resultErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return nil, errors.Join(errors.New("invalid policy descriptor"), unix.Close(fd))
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("policy source changed before read")
	}
	data, err = task.ReadBounded(f, task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	if err = verifyFileAfterRead(path, f, opened, int64(len(data))); err != nil {
		return nil, err
	}
	return data, nil
}

func verifyFileAfterRead(path string, f *os.File, opened os.FileInfo, length int64) error {
	final, err := f.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() || !os.SameFile(named, opened) || !os.SameFile(final, opened) ||
		final.Size() != opened.Size() || length != opened.Size() || !final.ModTime().Equal(opened.ModTime()) {
		return errors.New("policy source changed during read")
	}
	return nil
}
