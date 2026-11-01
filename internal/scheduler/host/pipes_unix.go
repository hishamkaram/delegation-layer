//go:build linux || darwin

package host

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Inherited ExtraFiles begin blocking and without CLOEXEC. Mark each original
// end CLOEXEC and nonblocking before NewFile so Close interrupts a blocked Read
// and later subprocesses cannot retain the host channel.
func inheritedPipe(fd int, name string, access int) (*os.File, error) {
	if fd < 3 {
		return nil, fmt.Errorf("%w: invalid inherited pipe", ErrIdentity)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != access {
		return nil, fmt.Errorf("%w: inherited descriptor is not the requested pipe", ErrIdentity)
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		return nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, errors.Join(os.ErrInvalid, unix.Close(fd))
	}
	return file, nil
}
