package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Supported default and mode constants.
const (
	DefaultBudgetDuration = 30 * time.Minute
	DefaultMode           = "read-only"

	ModeReadOnly       = "read-only"
	ModeWorkspaceWrite = "workspace-write"

	ProviderAntigravityPrint = "antigravity:print"
	ProviderCodexExec        = "codex:exec"
	ProviderClaudePrint      = "claude:print"
	ProviderFixture          = "fixture:test"
)

// Sentinel configuration errors.
var (
	ErrInvalidBudget        = errors.New("budget must be a finite positive duration")
	ErrInvalidMode          = errors.New("invalid permission mode: must be 'read-only' or 'workspace-write'")
	ErrUnsupportedProvider  = errors.New("unsupported provider")
	ErrInvalidCwd           = errors.New("working directory must be an absolute path to an existing directory")
	ErrRootWorkspaceOverlap = errors.New("state root and workspace directory must be disjoint and must not overlap in either direction")
	ErrTokenBudgetRejected  = errors.New("token and dollar budget ceilings are not supported; wall-clock budget is the sole enforced limit")
	ErrRawArgvRejected      = errors.New("raw argv passthrough is forbidden")
	ErrBriefNotFound        = errors.New("brief file not found")
	ErrBriefNotRegular      = errors.New("brief must be a regular file")
	ErrBriefTooLarge        = errors.New("brief file exceeds 8 MiB limit")
)

// ParseBudget parses and validates a budget string, ensuring a finite positive duration.
func ParseBudget(budgetStr string) (time.Duration, error) {
	if budgetStr == "" {
		return DefaultBudgetDuration, nil
	}
	d, err := time.ParseDuration(budgetStr)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidBudget, err)
	}
	if d <= 0 {
		return 0, ErrInvalidBudget
	}
	return d, nil
}

// ValidateMode validates that mode is a recognized permission mode.
func ValidateMode(mode string) error {
	if mode == "" {
		mode = DefaultMode
	}
	switch mode {
	case ModeReadOnly, ModeWorkspaceWrite:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}
}

// ValidateProvider validates the structural provider:transport identifier.
// Catalogs decide whether a structurally valid identifier is executable
// support; config deliberately does not keep a provider allowlist.
func ValidateProvider(provider string) error {
	name, transport, found := strings.Cut(provider, ":")
	if !found || strings.ContainsRune(transport, ':') || !validProviderComponent(name) || !validProviderComponent(transport) {
		return fmt.Errorf("%w: %q", ErrUnsupportedProvider, provider)
	}
	return nil
}

func validProviderComponent(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

// ResolveDefaultRoot returns the default persistent state root path.
func ResolveDefaultRoot() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving user config dir: %w", err)
	}
	return filepath.Join(configDir, "delegation-layer"), nil
}

// CanonicalizePath validates that p is an absolute path and resolves any symlinks,
// including symlinks in existing parent directories if the path itself does not exist yet.
func CanonicalizePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute: %s", p)
	}
	clean := filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return canonicalExistingPath(resolved)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolving symlinks for %s: %w", clean, err)
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return "", err
	}
	resolvedParent, err := CanonicalizePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(clean)), nil
}

// canonicalExistingPath recovers actual directory-entry spelling. EvalSymlinks
// alone can preserve case aliases on case-insensitive filesystems.
func canonicalExistingPath(p string) (string, error) {
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	canonicalParent, err := canonicalExistingPath(parent)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(canonicalParent)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(p) {
			return filepath.Join(canonicalParent, entry.Name()), nil
		}
	}
	return canonicalAlias(canonicalParent, entries, info)
}

func canonicalAlias(parent string, entries []os.DirEntry, info os.FileInfo) (string, error) {
	for _, entry := range entries {
		candidate, err := entry.Info()
		if err != nil {
			return "", err
		}
		if os.SameFile(info, candidate) {
			return filepath.Join(parent, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("canonical directory entry disappeared in %s", parent)
}

// ValidateDirectories verifies that root and cwd are canonical, cwd exists, and root and cwd do not overlap.
func ValidateDirectories(root, cwd string) (canonicalRoot, canonicalCwd string, err error) {
	canonicalRoot, err = CanonicalizePath(root)
	if err != nil {
		return "", "", fmt.Errorf("canonicalizing state root: %w", err)
	}
	canonicalCwd, err = CanonicalizePath(cwd)
	if err != nil {
		return "", "", fmt.Errorf("canonicalizing workspace cwd: %w", err)
	}

	info, err := os.Stat(canonicalCwd)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrInvalidCwd, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: %s is not a directory", ErrInvalidCwd, canonicalCwd)
	}

	if pathsOverlap(canonicalRoot, canonicalCwd) {
		return "", "", fmt.Errorf("%w: root=%s, cwd=%s", ErrRootWorkspaceOverlap, canonicalRoot, canonicalCwd)
	}

	return canonicalRoot, canonicalCwd, nil
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func pathsOverlap(p1, p2 string) bool {
	return isSubpath(p1, p2) || isSubpath(p2, p1)
}

// MaxBriefBytes is the global maximum size of brief.md (8 MiB).
const MaxBriefBytes = 8 * 1024 * 1024

// ValidateBriefFile verifies that path points to an existing regular file within the 8 MiB limit.
func ValidateBriefFile(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrBriefNotFound
		}
		return 0, fmt.Errorf("checking brief file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("%w: brief cannot be a symlink", ErrBriefNotRegular)
	}
	if !info.Mode().IsRegular() {
		return 0, ErrBriefNotRegular
	}
	if info.Size() > MaxBriefBytes {
		return 0, ErrBriefTooLarge
	}
	return info.Size(), nil
}
