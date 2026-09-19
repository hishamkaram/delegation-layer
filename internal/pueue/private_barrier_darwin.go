//go:build darwin

package pueue

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func privateBarrierFile(file *os.File) error {
	if err := file.Sync(); err != nil {
		return err
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}

func privateBarrierDir(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open private supervisor directory barrier: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	// F_FULLFSYNC is a file-data barrier and is not supported for directory
	// descriptors on macOS. fsync is the directory metadata barrier here.
	return directory.Sync()
}
