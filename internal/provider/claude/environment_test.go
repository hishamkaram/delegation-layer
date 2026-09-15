package claude

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEnvironmentPreservesNativeLoginAndDropsUnrelatedValues(t *testing.T) {
	home := t.TempDir()
	input := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "USER=fixture", "LOGNAME=fixture", "LANG=en_US.UTF-8", "UNRELATED_SECRET=fixture-secret", "DYLD_INSERT_LIBRARIES=/untrusted/library", "NODE_OPTIONS=--require=/untrusted/module"}
	environment, err := prepareEnvironment(input)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment.Values, "HOME="+home) || !slices.Contains(environment.Values, "PATH=/usr/bin:/bin") {
		t.Fatal("native home/path changed")
	}
	if !slices.Contains(environment.Values, storageBackendPin) {
		t.Fatal("native storage backend is not pinned")
	}
	for _, value := range environment.Values {
		if strings.Contains(value, "fixture-secret") || strings.HasPrefix(value, "DYLD_") || strings.HasPrefix(value, "NODE_") {
			t.Fatal("unrelated environment inherited")
		}
	}
	if !slices.IsSorted(environment.Values) || !slices.IsSorted(environment.WritableRoots) {
		t.Fatal("environment is nondeterministic")
	}
	if !slices.Contains(environment.WritableRoots, environment.ClaudeHome) {
		t.Fatal("native runtime storage omitted")
	}
}

func TestStorageBackendPinCannotBeOverriddenOrDuplicated(t *testing.T) {
	home := t.TempDir()
	for _, value := range []string{"", "1", "true", "false", "unknown"} {
		if _, err := prepareEnvironment([]string{"HOME=" + home, "CLAUDE_CODE_HOVER_REST=" + value}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatal("unqualified backend selector accepted")
		}
	}
	environment, err := prepareEnvironment([]string{"HOME=" + home, storageBackendPin})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, value := range environment.Values {
		if value == storageBackendPin {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("storage pin count=%d", count)
	}
	if _, err = parseEnvironment(environment.Values); err != nil {
		t.Fatal("compiled native environment cannot be revalidated")
	}
}

func TestEnvironmentRejectsAlternateAuthenticationAndConfiguration(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_EVAL_CONFINED", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_REMOTE", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"} {
		t.Run(key, func(t *testing.T) {
			for _, value := range []string{"", "fixture-secret"} {
				_, err := prepareEnvironment([]string{"HOME=" + home, key + "=" + value})
				if !errors.Is(err, ErrUnsupportedProfile) {
					t.Fatalf("got %v", err)
				}
				if strings.Contains(err.Error(), "fixture-secret") {
					t.Fatal("rejected value exposed")
				}
			}
		})
	}
	for name, values := range map[string][]string{"duplicate": {"HOME=" + home, "HOME=" + home}, "malformed": {"HOME=" + home, "broken"}, "NUL": {"HOME=" + home, "OTHER=x\x00y"}, "missing HOME": {"PATH=/bin"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareEnvironment(values); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
