//go:build linux || darwin

package sshtransport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

func privateFile(file *os.File, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		return fmt.Errorf("delegate trust path must be private and owned by the current user (mode %o, uid %d)", stat.Mode&0o777, stat.Uid)
	}
	kind := stat.Mode & unix.S_IFMT
	if directory && kind != unix.S_IFDIR {
		return fmt.Errorf("delegate trust parent is not a directory")
	}
	if !directory && (kind != unix.S_IFREG || stat.Nlink != 1) {
		return fmt.Errorf("delegate trust file must be a single-link regular file")
	}
	return nil
}

func trustRoot(path string) (*os.Root, *os.File, error) {
	parent := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, nil, err
	}
	if canonical != filepath.Clean(parent) {
		return nil, nil, fmt.Errorf("delegate trust parent must not contain symlinks")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, nil, errors.Join(err, root.Close())
	}
	if err = privateFile(directory, true); err != nil {
		return nil, nil, errors.Join(err, directory.Close(), root.Close())
	}
	return root, directory, nil
}

func validateTrustPath(path string, allowAbsent bool) error {
	root, directory, err := trustRoot(path)
	if err != nil {
		return err
	}
	err = checkTrustEntry(root, filepath.Base(path), allowAbsent)
	return errors.Join(err, directory.Close(), root.Close())
}

func checkTrustEntry(root *os.Root, name string, allowAbsent bool) error {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) && allowAbsent {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("delegate trust entry must be a regular file without symlinks")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	return errors.Join(privateFile(file, false), file.Close())
}

func appendTrust(path, host string, key ssh.PublicKey) error {
	root, directory, err := trustRoot(path)
	if err != nil {
		return err
	}
	writeErr := appendTrustRooted(root, filepath.Base(path), host, key)
	return errors.Join(writeErr, directory.Sync(), directory.Close(), root.Close())
}

func appendTrustRooted(root *os.Root, name, host string, key ssh.PublicKey) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err = privateFile(file, false); err != nil {
		return errors.Join(err, file.Close())
	}
	_, writeErr := io.WriteString(file, knownhosts.Line([]string{host}, key)+"\n")
	return errors.Join(writeErr, file.Sync(), file.Close())
}
