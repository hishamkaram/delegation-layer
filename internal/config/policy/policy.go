// Package policy contains pure configuration defaults and structural rules.
package policy

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	DefaultMode = "read-only"

	ModeReadOnly       = "read-only"
	ModeWorkspaceWrite = "workspace-write"

	ProviderAntigravityPrint = "antigravity:print"
	ProviderCodexExec        = "codex:exec"
	ProviderCodexReview      = "codex:review"
	ProviderClaudePrint      = "claude:print"
	ProviderPiJSON           = "pi:json"
	ProviderOpenCodeRun      = "opencode:run"
	ProviderFixture          = "fixture:test"

	MaxBriefBytes = 8 * 1024 * 1024
)

var (
	ErrInvalidMode          = errors.New("invalid permission mode: must be 'read-only' or 'workspace-write'")
	ErrUnsupportedProvider  = errors.New("unsupported provider")
	ErrInvalidCwd           = errors.New("working directory must be an absolute path to an existing directory")
	ErrRootWorkspaceOverlap = errors.New("state root and workspace directory must be disjoint and must not overlap in either direction")
	ErrTokenBudgetRejected  = errors.New("token and dollar budget ceilings are not supported; wall-clock budget is the sole enforced limit")
	ErrRawArgvRejected      = errors.New("raw argv passthrough is forbidden")
	ErrBriefNotFound        = errors.New("brief file not found")
	ErrBriefNotRegular      = errors.New("brief must be a regular file")
	ErrBriefTooLarge        = errors.New("brief file exceeds 8 MiB limit")
	ErrBriefEmpty           = errors.New("brief must be nonempty")
)

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
// Catalogs decide whether a structurally valid identifier is executable support.
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

// PathsOverlap reports lexical containment of clean paths. Callers resolve aliases first.
func PathsOverlap(first, second string) bool {
	return isSubpath(first, second) || isSubpath(second, first)
}

func isSubpath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
