package opencode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestPrepareEnvironmentScopesXDGRootsToOpenCode(t *testing.T) {
	values := []string{
		"HOME=/tmp/opencode-home",
		"PATH=/bin",
		"XDG_CONFIG_HOME=/tmp/xdg-config",
		"XDG_DATA_HOME=/tmp/xdg-data",
		"XDG_STATE_HOME=/tmp/xdg-state",
		"XDG_CACHE_HOME=/tmp/xdg-cache",
	}
	environment, err := prepareEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/tmp/xdg-data", "/tmp/xdg-state", "/tmp/xdg-cache"} {
		canonicalRoot, canonicalErr := config.CanonicalizePath(root)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		canonicalChild, childErr := config.CanonicalizePath(filepath.Join(root, "opencode"))
		if childErr != nil {
			t.Fatal(childErr)
		}
		if slices.Contains(environment.WritableRoots, canonicalRoot) {
			t.Fatalf("shared XDG root was classified as writable: %q", canonicalRoot)
		}
		if !slices.Contains(environment.WritableRoots, canonicalChild) {
			t.Fatalf("OpenCode XDG child was not classified as writable: %q", canonicalChild)
		}
	}
	configChild, configChildErr := config.CanonicalizePath(filepath.Join("/tmp/xdg-config", "opencode"))
	if configChildErr != nil {
		t.Fatal(configChildErr)
	}
	if slices.Contains(environment.WritableRoots, configChild) {
		t.Fatalf("original XDG config child was classified as writable: %q", configChild)
	}
	delegationConfig, delegationErr := config.CanonicalizePath(filepath.Join("/tmp/xdg-config", "delegation-layer-opencode"))
	if delegationErr != nil {
		t.Fatal(delegationErr)
	}
	if !slices.Contains(environment.WritableRoots, delegationConfig) {
		t.Fatalf("delegation-owned OpenCode config root was not classified as writable: %q", delegationConfig)
	}
	if !slices.Contains(environment.Values, "XDG_CACHE_HOME=/tmp/xdg-cache") {
		t.Fatalf("XDG_CACHE_HOME was not preserved: %q", environment.Values)
	}
	home, homeErr := config.CanonicalizePath("/tmp/opencode-home")
	if homeErr != nil {
		t.Fatal(homeErr)
	}
	if !slices.Contains(environment.WritableRoots, filepath.Join(home, ".opencode")) {
		t.Fatal("native ~/.opencode configuration root was not classified as writable")
	}
}

func TestPrepareEnvironmentRejectsMalformedXDGHome(t *testing.T) {
	if _, err := prepareEnvironment([]string{"HOME=/tmp/opencode-home", "XDG_CONFIG_HOME=relative"}); err == nil {
		t.Fatal("relative XDG_CONFIG_HOME was accepted")
	}
}

func TestNativeEnvironmentPreservesExplicitDiscoverySelectorsOnly(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	values := []string{
		"HOME=" + home,
		"PATH=/usr/bin",
		"OPENCODE_CONFIG=" + configPath,
		"OPENCODE_CONFIG_DIR=" + filepath.Join(home, "config"),
		"OPENCODE_TUI_CONFIG=" + filepath.Join(home, "tui.json"),
		"OPENCODE_PERMISSION={\"*\":\"allow\"}",
		"OPENCODE_AUTO_SHARE=0",
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg-config"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "runtime"),
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"SSH_AUTH_SOCK=" + filepath.Join(home, "ssh-agent.sock"),
		"OPENCODE_CONFIG_CONTENT={\"secret\":\"should-not-be-copied\"}",
		"OPENCODE_AUTH_CONTENT={\"access_token\":\"should-not-be-copied\"}",
		"OPENCODE_FUTURE_SECRET=should-not-be-copied",
		"OPENAI_API_KEY=should-not-be-copied",
	}
	environment, err := prepareProfileEnvironment(values, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG", "OPENCODE_PERMISSION", "OPENCODE_AUTO_SHARE", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK"} {
		found := false
		for _, value := range environment.Values {
			if strings.HasPrefix(value, name+"=") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("native selector %s was dropped: %q", name, environment.Values)
		}
	}
	for _, unwanted := range []string{"OPENCODE_CONFIG_CONTENT=", "OPENCODE_AUTH_CONTENT=", "OPENCODE_FUTURE_SECRET=", "OPENAI_API_KEY="} {
		for _, value := range environment.Values {
			if strings.HasPrefix(value, unwanted) {
				t.Errorf("sensitive or unknown environment entered persisted launch: %q", value)
			}
		}
	}
}

func TestNativeEnvironmentRoundTripsRootSelectors(t *testing.T) {
	home := t.TempDir()
	values := []string{
		"HOME=" + home,
		"PATH=/usr/bin",
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"TMP=" + filepath.Join(home, "tmp-alt"),
		"TEMP=" + filepath.Join(home, "temp"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg-config"),
		"XDG_CONFIG_DIRS=" + filepath.Join(home, "config-a") + string(filepath.ListSeparator) + filepath.Join(home, "config-b"),
		"XDG_DATA_HOME=" + filepath.Join(home, "xdg-data"),
		"XDG_DATA_DIRS=" + filepath.Join(home, "data-a") + string(filepath.ListSeparator) + filepath.Join(home, "data-b"),
		"XDG_STATE_HOME=" + filepath.Join(home, "xdg-state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "xdg-cache"),
		"OPENCODE_CONFIG=" + filepath.Join(home, "config.json"),
		"OPENCODE_CONFIG_DIR=" + filepath.Join(home, "config-dir"),
		"OPENCODE_TUI_CONFIG=" + filepath.Join(home, "tui.json"),
		"OPENCODE_FUTURE_SECRET=must-not-round-trip",
	}
	initial, err := prepareProfileEnvironment(values, true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(initial.Values, func(value string) bool { return strings.HasPrefix(value, "OPENCODE_FUTURE_SECRET=") }) {
		t.Fatalf("unknown or secret environment entered queued values: %q", initial.Values)
	}
	reconstructed, err := prepareProfileEnvironment(initial.Values, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(initial.Values, reconstructed.Values) {
		t.Fatalf("native environment values changed after round trip: initial=%q reconstructed=%q", initial.Values, reconstructed.Values)
	}
	if !slices.Equal(initial.WritableRoots, reconstructed.WritableRoots) {
		t.Fatalf("native writable roots changed after round trip: initial=%q reconstructed=%q", initial.WritableRoots, reconstructed.WritableRoots)
	}
}

func TestNativeConfigFileDoesNotReserveDefaultStateRoot(t *testing.T) {
	home, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home, err = config.CanonicalizePath(home)
	if err != nil {
		t.Fatal(err)
	}
	sharedConfigDir := filepath.Join(home, ".config")
	configFile := filepath.Join(sharedConfigDir, "opencode.json")
	environment, err := prepareProfileEnvironment([]string{
		"HOME=" + home,
		"PATH=/usr/bin",
		"OPENCODE_CONFIG=" + configFile,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	canonicalConfigFile, err := config.CanonicalizePath(configFile)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSharedConfigDir, err := config.CanonicalizePath(sharedConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment.WritableRoots, canonicalConfigFile) {
		t.Fatalf("explicit config file was not retained as a writable root: %q", environment.WritableRoots)
	}
	if slices.Contains(environment.WritableRoots, canonicalSharedConfigDir) {
		t.Fatalf("shared config directory was reserved by explicit config file: %q", canonicalSharedConfigDir)
	}
	stateRoot := filepath.Join(sharedConfigDir, "delegation-layer")
	candidate := commonprovider.ProfileCandidate{
		Directory:     filepath.Join(home, "workspace"),
		WritableRoots: environment.WritableRoots,
	}
	if err := candidate.ValidateStatePlacement(stateRoot); err != nil {
		t.Fatalf("default state root overlapped explicit config file: %v", err)
	}
}

func TestDelegationConfigHomeIsIdempotent(t *testing.T) {
	got, err := delegationConfigHome("/tmp/opencode-home", "/tmp/xdg-config/delegation-layer-opencode")
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.CanonicalizePath("/tmp/xdg-config/delegation-layer-opencode")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("isolated config home=%q, want %q", got, want)
	}
}
