package codex

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

const (
	Version                = "0.154.0"
	inspectedRuntimeSHA256 = "4f85982624b3898c8991cb80c0981b2aa71070e3537046c9a95950318a95afcc"
)

func resolveExecutable() (string, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", fmt.Errorf("%w: Codex runtime has not been inspected on this platform", ErrUnsupportedProfile)
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("%w: Codex executable is unavailable", ErrUnsupportedProfile)
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
		return "", fmt.Errorf("%w: inspect Codex runtime: %w", ErrUnsupportedProfile, err)
	}
	if digest != inspectedRuntimeSHA256 {
		return "", fmt.Errorf("%w: Codex executable differs from inspected 0.154.0 runtime", ErrUnsupportedProfile)
	}
	return path, nil
}
