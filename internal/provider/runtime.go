package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"golang.org/x/sys/unix"
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
	absolute, err := filepath.Abs(path)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("resolve provider executable: %w", err)
	}
	canonical, err := config.CanonicalizePath(absolute)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("resolve provider executable: %w", err)
	}
	digest, err := FingerprintExecutable(canonical)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("inspect provider executable: %w", err)
	}
	return CLIInfo{Path: canonical, SHA256: digest}, nil
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
