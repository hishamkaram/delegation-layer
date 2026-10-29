//go:build linux

package verifiedexec

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// filesystemFacts reports the filesystem type and the mount flags that decide
// whether a file below path may be executed.
func filesystemFacts(path string) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("type=0x%x flags=0x%x noexec=%t nosuid=%t", stat.Type, stat.Flags,
		stat.Flags&unix.ST_NOEXEC != 0, stat.Flags&unix.ST_NOSUID != 0), nil
}

// codeSignatureFacts has no meaning for ELF; it states that explicitly.
func codeSignatureFacts(string) string { return "not-applicable-on-linux" }
