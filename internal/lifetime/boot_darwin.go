//go:build darwin

package lifetime

import (
	"errors"

	"golang.org/x/sys/unix"
)

// BootID returns the kernel boot-session UUID. Unlike kern.boottime, this
// identity does not change when the system wall clock is adjusted.
func BootID() (string, error) {
	value, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	uuid, valid := canonicalBootUUID(value)
	if !valid {
		return "", errors.New("lifetime: invalid Darwin boot-session UUID")
	}
	return "darwin:boot-session:" + uuid, nil
}
