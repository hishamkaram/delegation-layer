//go:build linux || darwin

package remotefiles

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func singleLink(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("native file link count is unavailable")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("hard-linked files are unsupported")
	}
	return nil
}

// The private stage owner serializes writes. O_NOFOLLOW also rejects a final
// symlink if a path is substituted between validation and open.
func openWritable(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
}

func openReadOnly(root *os.Root, name string) (*os.File, error) {
	// A concurrent FIFO replacement must not block before the opened native
	// facts can be checked. O_NONBLOCK has no effect on regular file reads.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// DirectoryIdentity is an observation used to refuse replacement of an owned
// download directory. It is not a permanent identity across remounts or reboot.
type DirectoryIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func ObserveDirectory(root *os.Root) (DirectoryIdentity, error) {
	file, err := root.Open(".")
	if err != nil {
		return DirectoryIdentity{}, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if err = errors.Join(statErr, closeErr); err != nil {
		return DirectoryIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return DirectoryIdentity{}, fmt.Errorf("directory native identity unavailable")
	}
	return DirectoryIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

// RequirePrivateDirectory validates the local byte owner's native permissions.
func RequirePrivateDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if err = errors.Join(statErr, file.Close()); err != nil {
		return err
	}
	native, ok := info.Sys().(*syscall.Stat_t)
	if !ok || native.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("directory must be private and owned by the current user")
	}
	return nil
}
