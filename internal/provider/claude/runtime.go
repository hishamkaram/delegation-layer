package claude

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const (
	inspectedRuntimeSHA256 = "a506b6d970a4cf44f6abdb53a81ddcd5d3b0ce042a95c502fe9d1f946bdb8807"
)

func resolveExecutable() (string, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", fmt.Errorf("%w: Claude runtime has not been inspected on this platform", ErrUnsupportedProfile)
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("%w: Claude executable is unavailable", ErrUnsupportedProfile)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	digest, err := commonprovider.FingerprintExecutable(path)
	if err != nil {
		return "", fmt.Errorf("%w: inspect Claude runtime: %w", ErrUnsupportedProfile, err)
	}
	if digest != inspectedRuntimeSHA256 {
		return "", fmt.Errorf("%w: Claude executable differs from inspected 2.1.270 runtime", ErrUnsupportedProfile)
	}
	return path, nil
}
