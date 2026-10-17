//go:build darwin

package control

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
	// F_FULLFSYNC requests a device-level flush for buffered file data. Keep
	// directory metadata durability on the directory's fsync barrier here.
	return directory.Sync()
}
