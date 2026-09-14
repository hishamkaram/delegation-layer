package antigravity

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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

func hashRuntimeExecutable(path string) (string, error) {
	return commonprovider.FingerprintExecutable(path)
}
