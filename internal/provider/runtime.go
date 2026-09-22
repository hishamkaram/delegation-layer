package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// CLIInfo describes the executable selected for one task. The version and
// digest are observations used to bind admission to the same executable; they
// are observations only; they are never compared with a release manifest.
type CLIInfo struct {
	Path    string
	Version string
	SHA256  string
}

// LocateCLI resolves and fingerprints a provider executable without starting
// it. Version and help output are observed later by the supervised inspection
// worker immediately before the provider launch.
func LocateCLI(name string) (CLIInfo, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("provider executable %q is unavailable: %w", name, err)
	}
	return LocateCLIPath(path)
}

// LocateCLIPath applies the static executable checks used before a supervised
// capability probe. It rejects aliases, special files, and non-executable
// paths while never invoking the provider.
func LocateCLIPath(path string) (CLIInfo, error) {
	info, err := verifiedexec.LocatePath(path)
	if err != nil {
		return CLIInfo{}, err
	}
	return CLIInfo{Path: info.Path, SHA256: info.SHA256}, nil
}

// ParseCLIVersion accepts any nonempty, valid UTF-8 text emitted on stdout by
// a provider's --version command. It intentionally performs no release check.
func ParseCLIVersion(output []byte) (string, error) {
	if len(output) == 0 || !utf8.Valid(output) {
		return "", errors.New("version output is empty or not valid UTF-8")
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", errors.New("version output is empty")
	}
	for _, character := range version {
		if unicode.IsControl(character) {
			return "", errors.New("version output contains control characters")
		}
	}
	return version, nil
}

func parseCLIVersion(output []byte) (string, error) { return ParseCLIVersion(output) }

func containsCLIFlag(output []byte, flag string) bool {
	if flag == "" {
		return false
	}
	text := string(output)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], flag)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(flag)
		if (index == 0 || !isCLIFlagRune(text[index-1])) && (end == len(text) || !isCLIFlagRune(text[end])) {
			return true
		}
		offset = end
	}
	return false
}

// ContainsCLIFlag reports whether a help stream advertises one complete flag
// token. It deliberately accepts output from either stdout or stderr because
// CLIs differ in where they print help diagnostics.
func ContainsCLIFlag(output []byte, flag string) bool { return containsCLIFlag(output, flag) }

func isCLIFlagRune(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '-' || value == '_'
}

// FingerprintExecutable reads a canonical regular executable without launching
// it. The digest is an admission identity observation, not a release
// constraint.
func FingerprintExecutable(path string) (digest string, resultErr error) {
	return verifiedexec.Fingerprint(path)
}

// OpenVerifiedExecutable opens the exact regular executable represented by
// expectedSHA256 and leaves the descriptor positioned at its beginning. The
// descriptor can be used by platform-specific launch code that supports
// descriptor-backed execution; portable callers should use the verified
// command builder, which preserves the same admitted bytes on every platform.
func OpenVerifiedExecutable(path, expectedSHA256 string) (*os.File, error) {
	return verifiedexec.Open(path, expectedSHA256)
}
