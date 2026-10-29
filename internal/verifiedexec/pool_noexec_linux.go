//go:build linux

package verifiedexec

import "golang.org/x/sys/unix"

// filesystemForbidsExec reports whether the mount holding path is noexec.
func filesystemForbidsExec(path string) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false, err
	}
	return stat.Flags&unix.ST_NOEXEC != 0, nil
}
