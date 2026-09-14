package task

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPolicySources bounds the number of immutable policy source observations
	// that can be carried by one effective configuration.
	MaxPolicySources = 512
	// MaxPolicyWritableRoots bounds the provider-writable roots carried by one
	// effective configuration.
	MaxPolicyWritableRoots = 64
)

// PolicyDetails records the non-secret policy decisions and the exact source
// observations used to resolve them.
type PolicyDetails struct {
	ProfileRevision string               `json:"profile_revision"`
	RuntimeSHA256   string               `json:"runtime_sha256"`
	Workspace       string               `json:"workspace"`
	WritableRoots   []string             `json:"writable_roots,omitempty"`
	Sources         []PolicySourceDigest `json:"sources,omitempty"`
}

// PolicySourceDigest identifies one policy input and records whether it was
// present when the effective policy was resolved.
type PolicySourceDigest struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
}

// ValidateEffectiveConfig validates both the historical effective fields and
// the optional persisted policy details.
func ValidateEffectiveConfig(config EffectiveConfig) error {
	if err := ValidateSHA256(config.Digest); err != nil {
		return fmt.Errorf("effective config digest: %w", err)
	}
	if config.Policy == nil {
		return nil
	}
	return validatePolicyDetails(*config.Policy)
}

func validatePolicyDetails(policy PolicyDetails) error {
	if err := validatePolicyText(policy.ProfileRevision, "policy profile revision", true); err != nil {
		return err
	}
	if err := ValidateSHA256(policy.RuntimeSHA256); err != nil {
		return fmt.Errorf("policy runtime SHA-256: %w", err)
	}
	if err := validatePolicyPath(policy.Workspace, "policy workspace"); err != nil {
		return err
	}
	if len(policy.WritableRoots) > MaxPolicyWritableRoots {
		return errors.New("policy writable_roots exceeds the maximum permitted count")
	}
	if err := validateWritableRoots(policy.WritableRoots); err != nil {
		return err
	}
	if len(policy.Sources) > MaxPolicySources {
		return errors.New("policy sources exceeds the maximum permitted count")
	}
	return validatePolicySources(policy.Sources)
}

func validateWritableRoots(roots []string) error {
	for index, root := range roots {
		if err := validatePolicyPath(root, "policy writable root"); err != nil {
			return fmt.Errorf("policy writable root %d: %w", index, err)
		}
		if index > 0 && root <= roots[index-1] {
			return errors.New("policy writable_roots must be strictly ordered and unique")
		}
	}
	return nil
}

func validatePolicySources(sources []PolicySourceDigest) error {
	for index, source := range sources {
		if err := validatePolicyPath(source.Path, "policy source path"); err != nil {
			return fmt.Errorf("policy source %d: %w", index, err)
		}
		if err := validatePolicyText(source.Kind, "policy source kind", true); err != nil {
			return fmt.Errorf("policy source %d: %w", index, err)
		}
		if index > 0 && !sourceIdentityAfter(sources[index-1], source) {
			return errors.New("policy sources must be strictly ordered and unique by path and kind")
		}
		if source.Present {
			if err := ValidateSHA256(source.SHA256); err != nil {
				return fmt.Errorf("policy source %d SHA-256: %w", index, err)
			}
		} else if source.SHA256 != "" {
			return fmt.Errorf("policy source %d absent source must have an empty SHA-256", index)
		}
	}
	return nil
}

func sourceIdentityAfter(previous, current PolicySourceDigest) bool {
	return current.Path > previous.Path || (current.Path == previous.Path && current.Kind > previous.Kind)
}

func validatePolicyPath(value, field string) error {
	if err := validatePolicyText(value, field, true); err != nil {
		return err
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", field)
	}
	return nil
}

func validatePolicyText(value, field string, required bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must not contain NUL", field)
	}
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be nonblank", field)
	}
	return nil
}

// CompareEffectiveConfigs compares effective configurations by value. Empty
// and nil optional policy collections have the same semantic value.
func CompareEffectiveConfigs(a, b EffectiveConfig) bool {
	if a.Containment != b.Containment || a.Approval != b.Approval || a.Digest != b.Digest {
		return false
	}
	if a.Policy == nil || b.Policy == nil {
		return a.Policy == nil && b.Policy == nil
	}
	return comparePolicyDetails(*a.Policy, *b.Policy)
}

func comparePolicyDetails(a, b PolicyDetails) bool {
	return a.ProfileRevision == b.ProfileRevision &&
		a.RuntimeSHA256 == b.RuntimeSHA256 &&
		a.Workspace == b.Workspace &&
		slices.Equal(a.WritableRoots, b.WritableRoots) &&
		slices.Equal(a.Sources, b.Sources)
}

// CloneEffectiveConfig returns an independent copy of an effective
// configuration, including all optional policy slices.
func CloneEffectiveConfig(config EffectiveConfig) EffectiveConfig {
	clone := config
	if config.Policy == nil {
		return clone
	}
	policy := *config.Policy
	policy.WritableRoots = slices.Clone(config.Policy.WritableRoots)
	policy.Sources = slices.Clone(config.Policy.Sources)
	clone.Policy = &policy
	return clone
}
