//go:build linux

package taskdir

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrUnsupportedFilesystem indicates that the storage filesystem policy was violated.
var ErrUnsupportedFilesystem = errors.New("unsupported filesystem: state root must reside on a supported persistent local filesystem")

const (
	nfsMagic  = 0x6969
	smbMagic  = 0x517B
	cifsMagic = 0xFF534D42
)

func checkPlatformFilesystemPolicy(rootPath string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(rootPath, &stat); err != nil {
		return fmt.Errorf("statfs %s: %w", rootPath, err)
	}
	return checkLinuxFilesystemType(uint32(stat.Type))
}

func checkLinuxFilesystemType(kind uint32) error {
	switch kind {
	case 0xef53, 0x58465342, 0x9123683e: // ext family (ext4 integration profile), XFS, Btrfs
		return nil
	default:
		return fmt.Errorf("%w: unapproved filesystem magic 0x%x", ErrUnsupportedFilesystem, kind)
	}
}
