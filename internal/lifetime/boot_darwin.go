//go:build darwin

package lifetime

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"
)

// BootID returns the kernel boot time used to reject stale process-scope
// evidence after a restart.
func BootID() (string, error) {
	value, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	if value == nil || value.Sec <= 0 || value.Usec < 0 {
		return "", errors.New("lifetime: invalid Darwin boot identity")
	}
	return "darwin:" + strconv.FormatInt(value.Sec, 10) + ":" + strconv.FormatInt(int64(value.Usec), 10), nil
}
