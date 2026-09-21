package opencode

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
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
