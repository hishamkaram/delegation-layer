package fakesupervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func writeAtomic(path string, data []byte, mode os.FileMode, maxBytes int) (resultErr error) {
	if len(data) > maxBytes {
		return fmt.Errorf("%w: atomic record exceeds %d bytes", ErrOutputTooBig, maxBytes)
	}
	parent := filepath.Dir(path)
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".fake-supervisor-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, tmp.Close())
		}
		if resultErr != nil {
			removeErr := os.Remove(tmpPath)
			if !errors.Is(removeErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, removeErr)
			}
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = writeFull(tmp, data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	err = tmp.Close()
	closed = true
	if err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err = syncDirectory(parent); err != nil {
		return err
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return fmt.Errorf("short atomic write: %d bytes", n)
		}
		data = data[n:]
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := ensureDirectory(path); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if canonical != path {
		return fmt.Errorf("%w: artifact directory must be canonical", ErrInvalidConfig)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: artifact directory must be private", ErrInvalidConfig)
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: output parent must be a directory", ErrInvalidConfig)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
