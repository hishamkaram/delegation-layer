package claude

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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

func TestNativeEnvironmentKeepsDiscoverySessionPathsWithoutSecrets(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(home, "config")
	dataHome := filepath.Join(home, "data")
	runtimeDir := filepath.Join(home, "runtime")
	input := []string{
		"HOME=" + home, "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=" + configHome,
		"XDG_DATA_HOME=" + dataHome, "XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtimeDir, "bus"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude-config"),
		"CLAUDE_CODE_OAUTH_TOKEN=fixture-secret", "ANTHROPIC_API_KEY=fixture-secret",
		"CLAUDE_CODE_HOVER_REST=1", "UNRELATED_SECRET=fixture-secret",
	}
	environment, err := prepareNativeEnvironment(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"XDG_CONFIG_HOME=" + configHome, "XDG_DATA_HOME=" + dataHome, "XDG_RUNTIME_DIR=" + runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(runtimeDir, "bus"), "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude-config")} {
		if !slices.Contains(environment.Values, want) {
			t.Fatalf("native discovery value %q was dropped: %v", want, environment.Values)
		}
	}
	if slices.Contains(environment.Values, storageBackendPin) {
		t.Fatal("native environment retained the historical storage pin")
	}
	for _, value := range environment.Values {
		if strings.Contains(value, "fixture-secret") {
			t.Fatal("native environment copied a secret")
		}
	}
	if slices.Contains(environment.WritableRoots, configHome) {
		t.Fatalf("shared XDG config root was classified as writable: %v", environment.WritableRoots)
	}
	if !slices.Contains(environment.WritableRoots, filepath.Join(configHome, "claude")) ||
		!slices.Contains(environment.WritableRoots, filepath.Join(home, "claude-config")) ||
		!slices.Contains(environment.WritableRoots, environment.ClaudeHome) {
		t.Fatalf("native discovery roots=%v", environment.WritableRoots)
	}
}

func TestNativeEnvironmentScopesXDGRootsAndAdmitsDefaultState(t *testing.T) {
	home := "/fixture/home"
	configHome := filepath.Join(home, "config:personal")
	configA := filepath.Join(home, "config-a")
	configB := filepath.Join(home, "config-b")
	dataHome := filepath.Join(home, "data")
	dataA := filepath.Join(home, "data-a")
	dataB := filepath.Join(home, "data-b")
	stateHome := filepath.Join(home, "state")
	cacheHome := filepath.Join(home, "cache")
	input := []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"XDG_CONFIG_HOME=" + configHome,
		"XDG_CONFIG_DIRS=" + configA + string(filepath.ListSeparator) + configB,
		"XDG_DATA_HOME=" + dataHome,
		"XDG_DATA_DIRS=" + dataA + string(filepath.ListSeparator) + dataB,
		"XDG_STATE_HOME=" + stateHome,
		"XDG_CACHE_HOME=" + cacheHome,
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "runtime"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude-config"),
	}
	environment, err := prepareNativeEnvironment(input)
	if err != nil {
		t.Fatal(err)
	}
	shared := []string{configHome, configA, configB, dataHome, dataA, dataB, stateHome, cacheHome}
	for _, root := range shared {
		if slices.Contains(environment.WritableRoots, root) {
			t.Fatalf("shared XDG root was classified as writable: %q in %q", root, environment.WritableRoots)
		}
	}
	for _, root := range []string{
		filepath.Join(configHome, "claude"), filepath.Join(configA, "claude"), filepath.Join(configB, "claude"),
		filepath.Join(dataHome, "claude"), filepath.Join(dataA, "claude"), filepath.Join(dataB, "claude"),
		filepath.Join(stateHome, "claude"), filepath.Join(cacheHome, "claude"), filepath.Join(home, "claude-config"),
	} {
		if !slices.Contains(environment.WritableRoots, root) {
			t.Fatalf("provider-owned discovery root was omitted: %q in %q", root, environment.WritableRoots)
		}
	}
	if slices.Contains(environment.WritableRoots, filepath.Join(home, "runtime")) {
		t.Fatalf("shared XDG runtime root was classified as writable: %q", environment.WritableRoots)
	}
	defaultState := filepath.Join(configHome, "delegation-layer")
	profile := commonprovider.PreparedProfile{
		Plan:          execution.Plan{Directory: filepath.Join(home, "workspace")},
		WritableRoots: environment.WritableRoots,
	}
	if err := profile.ValidateStatePlacement(defaultState); err != nil {
		t.Fatalf("default delegation state was rejected by Claude roots: %v", err)
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
