//go:build darwin

package taskdir

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrUnsupportedFilesystem indicates that the storage filesystem policy was violated.
var ErrUnsupportedFilesystem = errors.New("unsupported filesystem: state root must reside on a supported persistent local filesystem (apfs/hfs)")

func checkPlatformFilesystemPolicy(rootPath string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(rootPath, &stat); err != nil {
		return fmt.Errorf("statfs %s: %w", rootPath, err)
	}
	fstype := unix.ByteSliceToString(stat.Fstypename[:])
	switch fstype {
	case "apfs", "hfs":
		return nil
	default:
		return fmt.Errorf("%w: detected %q on %s", ErrUnsupportedFilesystem, fstype, rootPath)
	}
}
