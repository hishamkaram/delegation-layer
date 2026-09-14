package antigravity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"
)

// This exact executable was inspected with --version and --help. Binary
// identity is not permission certification; the native acceptance gate must
// separately qualify its fixed profile before a release can enable dispatch.
const inspectedDarwinARM64SHA256 = "cabadc15a61944372bede1fdff186701c17467dd9d718e97dc79283055d3c101"

type runtimeIdentity struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

func resolveRuntime() (runtimeIdentity, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return runtimeIdentity{}, fmt.Errorf("%w: agy runtime has not been verified on this platform", ErrUnsupportedProfile)
	}
	path, err := exec.LookPath("agy")
	if err != nil {
		return runtimeIdentity{}, fmt.Errorf("%w: agy executable is unavailable", ErrUnsupportedProfile)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return runtimeIdentity{}, err
	}
	canonical, err := config.CanonicalizePath(absolute)
	if err != nil {
		return runtimeIdentity{}, err
	}
	digest, err := hashRuntimeExecutable(canonical)
	if err != nil {
		return runtimeIdentity{}, fmt.Errorf("%w: cannot verify agy executable: %w", ErrUnsupportedProfile, err)
	}
	if digest != inspectedDarwinARM64SHA256 {
		return runtimeIdentity{}, fmt.Errorf("%w: agy executable differs from the inspected 1.2.2 runtime", ErrUnsupportedProfile)
	}
	return runtimeIdentity{Executable: canonical, Version: Version, SHA256: digest, OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
}

func hashRuntimeExecutable(path string) (digest string, resultErr error) {
	canonical, err := config.CanonicalizePath(path)
	if err != nil || canonical != path {
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
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return "", errors.Join(errors.New("invalid executable descriptor"), unix.Close(fd))
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	return hashOpenedRuntime(path, f, before)
}

func hashOpenedRuntime(path string, f *os.File, before os.FileInfo) (string, error) {
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o111 == 0 || !os.SameFile(before, opened) {
		return "", errors.New("runtime changed before inspection")
	}
	hash := sha256.New()
	length, err := io.Copy(hash, f)
	if err != nil {
		return "", err
	}
	if err = checkPolicyFileAfterRead(path, f, opened, length); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
