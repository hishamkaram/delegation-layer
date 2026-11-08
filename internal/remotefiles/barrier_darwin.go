//go:build darwin

package remotefiles

import (
	"os"

	"golang.org/x/sys/unix"
)

func barrierFile(file *os.File) error {
	if err := file.Sync(); err != nil {
		return err
	}
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
