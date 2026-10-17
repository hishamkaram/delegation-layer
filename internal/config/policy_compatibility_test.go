package config

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

func TestPolicyCompatibilityAliasesPreserveConstantsAndErrorIdentity(t *testing.T) {
	durationConstants := []struct {
		name   string
		legacy time.Duration
		pure   time.Duration
	}{
		{name: "default budget", legacy: DefaultBudgetDuration, pure: policy.DefaultBudgetDuration},
		{name: "review budget", legacy: DefaultCodexReviewBudgetDuration, pure: policy.DefaultCodexReviewBudgetDuration},
	}
	for _, tc := range durationConstants {
		if tc.legacy != tc.pure {
			t.Errorf("legacy %s constant = %v, policy = %v", tc.name, tc.legacy, tc.pure)
		}
	}
	stringConstants := []struct {
		name   string
		legacy string
		pure   string
	}{
		{name: "default mode", legacy: DefaultMode, pure: policy.DefaultMode},
		{name: "read-only mode", legacy: ModeReadOnly, pure: policy.ModeReadOnly},
		{name: "workspace-write mode", legacy: ModeWorkspaceWrite, pure: policy.ModeWorkspaceWrite},
		{name: "antigravity provider", legacy: ProviderAntigravityPrint, pure: policy.ProviderAntigravityPrint},
		{name: "codex execution provider", legacy: ProviderCodexExec, pure: policy.ProviderCodexExec},
		{name: "codex review provider", legacy: ProviderCodexReview, pure: policy.ProviderCodexReview},
		{name: "claude provider", legacy: ProviderClaudePrint, pure: policy.ProviderClaudePrint},
		{name: "Pi provider", legacy: ProviderPiJSON, pure: policy.ProviderPiJSON},
		{name: "OpenCode provider", legacy: ProviderOpenCodeRun, pure: policy.ProviderOpenCodeRun},
		{name: "fixture provider", legacy: ProviderFixture, pure: policy.ProviderFixture},
	}
	for _, tc := range stringConstants {
		if tc.legacy != tc.pure {
			t.Errorf("legacy %s constant = %q, policy = %q", tc.name, tc.legacy, tc.pure)
		}
	}
	if MaxBriefBytes != policy.MaxBriefBytes {
		t.Errorf("legacy brief size = %d, policy = %d", MaxBriefBytes, policy.MaxBriefBytes)
	}
	errorPairs := []struct {
		name   string
		legacy error
		pure   error
	}{
		{name: "invalid budget", legacy: ErrInvalidBudget, pure: policy.ErrInvalidBudget},
		{name: "invalid mode", legacy: ErrInvalidMode, pure: policy.ErrInvalidMode},
		{name: "unsupported provider", legacy: ErrUnsupportedProvider, pure: policy.ErrUnsupportedProvider},
		{name: "invalid cwd", legacy: ErrInvalidCwd, pure: policy.ErrInvalidCwd},
		{name: "root workspace overlap", legacy: ErrRootWorkspaceOverlap, pure: policy.ErrRootWorkspaceOverlap},
		{name: "token budget rejection", legacy: ErrTokenBudgetRejected, pure: policy.ErrTokenBudgetRejected},
		{name: "raw argv rejection", legacy: ErrRawArgvRejected, pure: policy.ErrRawArgvRejected},
		{name: "brief not found", legacy: ErrBriefNotFound, pure: policy.ErrBriefNotFound},
		{name: "brief not regular", legacy: ErrBriefNotRegular, pure: policy.ErrBriefNotRegular},
		{name: "brief too large", legacy: ErrBriefTooLarge, pure: policy.ErrBriefTooLarge},
	}
	for _, tc := range errorPairs {
		if !errors.Is(tc.legacy, tc.pure) || !errors.Is(tc.pure, tc.legacy) {
			t.Errorf("legacy %s error is not the policy sentinel", tc.name)
		}
	}
}

func TestBudgetCompatibilityForwardsPreserveDefaultsAndErrors(t *testing.T) {
	cases := []struct {
		provider string
		budget   string
		want     time.Duration
		wantErr  bool
	}{
		{budget: "", want: DefaultBudgetDuration},
		{provider: ProviderCodexReview, want: DefaultCodexReviewBudgetDuration},
		{provider: ProviderCodexExec, budget: "12m", want: 12 * time.Minute},
		{budget: "0s", wantErr: true},
	}
	for _, tc := range cases {
		legacy, legacyErr := ParseBudgetForProvider(tc.provider, tc.budget)
		pure, pureErr := policy.ParseBudgetForProvider(tc.provider, tc.budget)
		if legacy != pure || (legacyErr == nil) != (pureErr == nil) {
			t.Fatalf("budget results differ for provider=%q budget=%q: legacy=%v,%v policy=%v,%v", tc.provider, tc.budget, legacy, legacyErr, pure, pureErr)
		}
		if tc.wantErr {
			if !errors.Is(legacyErr, ErrInvalidBudget) || !errors.Is(pureErr, policy.ErrInvalidBudget) {
				t.Fatalf("invalid budget errors lost identity: legacy=%v policy=%v", legacyErr, pureErr)
			}
		} else if legacy != tc.want {
			t.Fatalf("budget result = %v, want %v", legacy, tc.want)
		}
	}
	legacy, legacyErr := ParseBudget("30s")
	pure, pureErr := policy.ParseBudget("30s")
	if legacy != pure || legacyErr != nil || pureErr != nil {
		t.Fatalf("ParseBudget forwarding differs: legacy=%v,%v policy=%v,%v", legacy, legacyErr, pure, pureErr)
	}
}

func TestModeCompatibilityForwardPreservesValidation(t *testing.T) {
	for _, mode := range []string{"", ModeReadOnly, ModeWorkspaceWrite, "invalid"} {
		legacyErr := ValidateMode(mode)
		pureErr := policy.ValidateMode(mode)
		if (legacyErr == nil) != (pureErr == nil) || legacyErr != nil && legacyErr.Error() != pureErr.Error() {
			t.Fatalf("ValidateMode(%q) differs: legacy=%v policy=%v", mode, legacyErr, pureErr)
		}
		if legacyErr != nil && (!errors.Is(legacyErr, ErrInvalidMode) || !errors.Is(legacyErr, policy.ErrInvalidMode)) {
			t.Fatalf("ValidateMode(%q) lost ErrInvalidMode identity: %v", mode, legacyErr)
		}
	}
}

func TestProviderCompatibilityForwardPreservesValidation(t *testing.T) {
	for _, provider := range []string{"openai:gpt-4.1", "openai", "OpenAI:exec"} {
		legacyErr := ValidateProvider(provider)
		pureErr := policy.ValidateProvider(provider)
		if (legacyErr == nil) != (pureErr == nil) || legacyErr != nil && legacyErr.Error() != pureErr.Error() {
			t.Fatalf("ValidateProvider(%q) differs: legacy=%v policy=%v", provider, legacyErr, pureErr)
		}
		if legacyErr != nil && (!errors.Is(legacyErr, ErrUnsupportedProvider) || !errors.Is(legacyErr, policy.ErrUnsupportedProvider)) {
			t.Fatalf("ValidateProvider(%q) lost ErrUnsupportedProvider identity: %v", provider, legacyErr)
		}
	}
}

func TestPathsOverlapCompatibilityForwardPreservesLexicalRule(t *testing.T) {
	cases := []struct {
		first  string
		second string
		want   bool
	}{
		{first: filepath.Join("state", "nested"), second: "state", want: true},
		{first: "state", second: filepath.Join("state", "nested"), want: true},
		{first: "state", second: "state-backup"},
	}
	for _, tc := range cases {
		legacy := PathsOverlap(tc.first, tc.second)
		pure := policy.PathsOverlap(tc.first, tc.second)
		if legacy != tc.want || pure != tc.want {
			t.Errorf("PathsOverlap(%q, %q) = legacy %t, policy %t; want %t", tc.first, tc.second, legacy, pure, tc.want)
		}
	}
}
