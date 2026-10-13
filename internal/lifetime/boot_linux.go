//go:build linux

package lifetime

import (
	"errors"
	"os"
	"strings"
)

const linuxBootIDPath = "/proc/sys/kernel/random/boot_id"

// BootID returns the kernel boot identity used to reject stale process-scope
// evidence after a restart.
func BootID() (string, error) {
	return readLinuxBootID(linuxBootIDPath)
}

func readLinuxBootID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n\t ") {
		return "", errors.New("lifetime: invalid Linux boot identity")
	}
	return "linux:" + value, nil
}
