package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"
)

// FingerprintExecutable reads a canonical regular executable without launching
// it. Adapters own version and certification decisions; this helper owns only
// the stable descriptor inspection shared by their preparation paths.
func FingerprintExecutable(path string) (digest string, resultErr error) {
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path || !filepath.IsAbs(path) {
		return "", errors.New("executable path is not canonical")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return "", errors.New("runtime is not a regular executable")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return "", errors.Join(errors.New("invalid executable descriptor"), unix.Close(fd))
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return fingerprintOpened(path, file, before)
}

func fingerprintOpened(path string, file *os.File, before os.FileInfo) (string, error) {
	opened, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o111 == 0 || !os.SameFile(before, opened) {
		return "", errors.New("runtime changed before inspection")
	}
	hash := sha256.New()
	length, err := io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	final, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	if err = errors.Join(statErr, nameErr); err != nil {
		return "", err
	}
	if !named.Mode().IsRegular() || named.Mode().Perm()&0o111 == 0 || !os.SameFile(named, opened) ||
		!os.SameFile(final, opened) || final.Size() != opened.Size() || length != opened.Size() ||
		!final.ModTime().Equal(opened.ModTime()) || !named.ModTime().Equal(opened.ModTime()) {
		return "", errors.New("runtime changed during inspection")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
