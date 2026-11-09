//go:build linux || darwin

package remotedata

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func privateOwner(info os.FileInfo) error {
	native, ok := info.Sys().(*syscall.Stat_t)
	if !ok || native.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("remote data directory must be private and owned by the current user")
	}
	return nil
}

func openDirectory(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func removeEmpty(parent *os.Root, name string) error {
	err := parent.Remove(name)
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
