package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

const (
	DefaultBudgetDuration            = policy.DefaultBudgetDuration
	DefaultCodexReviewBudgetDuration = policy.DefaultCodexReviewBudgetDuration
	DefaultMode                      = policy.DefaultMode
	ModeReadOnly                     = policy.ModeReadOnly
	ModeWorkspaceWrite               = policy.ModeWorkspaceWrite
	ProviderAntigravityPrint         = policy.ProviderAntigravityPrint
	ProviderCodexExec                = policy.ProviderCodexExec
	ProviderCodexReview              = policy.ProviderCodexReview
	ProviderClaudePrint              = policy.ProviderClaudePrint
	ProviderPiJSON                   = policy.ProviderPiJSON
	ProviderOpenCodeRun              = policy.ProviderOpenCodeRun
	ProviderFixture                  = policy.ProviderFixture
	MaxBriefBytes                    = policy.MaxBriefBytes
)

var (
	ErrInvalidBudget        = policy.ErrInvalidBudget
	ErrInvalidMode          = policy.ErrInvalidMode
	ErrUnsupportedProvider  = policy.ErrUnsupportedProvider
	ErrInvalidCwd           = policy.ErrInvalidCwd
	ErrRootWorkspaceOverlap = policy.ErrRootWorkspaceOverlap
	ErrTokenBudgetRejected  = policy.ErrTokenBudgetRejected
	ErrRawArgvRejected      = policy.ErrRawArgvRejected
	ErrBriefNotFound        = policy.ErrBriefNotFound
	ErrBriefNotRegular      = policy.ErrBriefNotRegular
	ErrBriefTooLarge        = policy.ErrBriefTooLarge
)

// ParseBudget preserves the legacy configuration package entry point.
func ParseBudget(budgetStr string) (time.Duration, error) {
	return policy.ParseBudget(budgetStr)
}

// ParseBudgetForProvider preserves the legacy configuration package entry point.
func ParseBudgetForProvider(provider, budgetStr string) (time.Duration, error) {
	return policy.ParseBudgetForProvider(provider, budgetStr)
}

// ValidateMode preserves the legacy configuration package entry point.
func ValidateMode(mode string) error {
	return policy.ValidateMode(mode)
}

// ValidateProvider preserves the legacy configuration package entry point.
func ValidateProvider(provider string) error {
	return policy.ValidateProvider(provider)
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

	if policy.PathsOverlap(canonicalRoot, canonicalCwd) {
		return "", "", fmt.Errorf("%w: root=%s, cwd=%s", ErrRootWorkspaceOverlap, canonicalRoot, canonicalCwd)
	}

	return canonicalRoot, canonicalCwd, nil
}

// PathsOverlap reports lexical containment of clean canonical paths. Callers
// must resolve filesystem aliases before using this to validate ownership.
func PathsOverlap(first, second string) bool { return policy.PathsOverlap(first, second) }

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
