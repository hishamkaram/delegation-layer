package pi

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func TestPrepareEnvironmentPreservesNativeDirectorySelectors(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, "pi-config")
	sessionDir := filepath.Join(home, "pi-sessions")
	environment, err := prepareEnvironment([]string{
		"HOME=" + home,
		piAgentDirEnv + "=" + agentDir,
		piSessionDirEnv + "=" + sessionDir,
		"PATH=/usr/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalAgentDir, err := config.CanonicalizePath(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSessionDir, err := config.CanonicalizePath(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if environment.AgentDir != canonicalAgentDir || environment.SessionDir != canonicalSessionDir {
		t.Fatalf("native directories=%+v", environment)
	}
	if !slices.Contains(environment.Values, piAgentDirEnv+"="+canonicalAgentDir) || !slices.Contains(environment.Values, piSessionDirEnv+"="+canonicalSessionDir) {
		t.Fatalf("selector environment was dropped: %q", environment.Values)
	}
	if !slices.Contains(environment.WritableRoots, canonicalAgentDir) || !slices.Contains(environment.WritableRoots, canonicalSessionDir) {
		t.Fatalf("selector roots were not bounded: %q", environment.WritableRoots)
	}
}

func TestPrepareEnvironmentPinsDefaultSessionDirectory(t *testing.T) {
	home := t.TempDir()
	environment, err := prepareEnvironment([]string{"HOME=" + home, "PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment.Values, piSessionDirEnv+"="+environment.SessionDir) {
		t.Fatalf("default session directory was not pinned: %q", environment.Values)
	}
}

func TestPrepareEnvironmentRejectsInvalidNativeDirectorySelectors(t *testing.T) {
	home := t.TempDir()
	for _, testCase := range []struct {
		name  string
		entry string
	}{
		{name: "relative config directory", entry: piAgentDirEnv + "=relative"},
		{name: "empty config directory", entry: piAgentDirEnv + "="},
		{name: "relative session directory", entry: piSessionDirEnv + "=relative"},
		{name: "empty session directory", entry: piSessionDirEnv + "="},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := prepareEnvironment([]string{"HOME=" + home, testCase.entry}); err == nil {
				t.Fatal("invalid native directory selector was accepted")
			}
		})
	}
}

func TestNativeEnvironmentPreservesDiscoveryEndpointsWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	values := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "runtime"),
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"PI_PRIVATE_TOKEN=private-fixture-secret",
	}
	got, err := prepareProfileEnvironment(values, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range values[1:4] {
		if !slices.Contains(got.Values, want) {
			t.Errorf("missing native discovery environment %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if slices.Contains(got.Values, values[4]) {
		t.Fatal("native credential value entered persisted environment")
	}
}

func TestNativeEnvironmentRoundTripsRuntimeAndSessionRoots(t *testing.T) {
	home := t.TempDir()
	values := []string{
		"HOME=" + home,
		"PI_CODING_AGENT_DIR=" + filepath.Join(home, "agent"),
		"PI_CODING_AGENT_SESSION_DIR=" + filepath.Join(home, "sessions"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "xdg-cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "runtime"),
		"GOCACHE=" + filepath.Join(home, "go-cache"),
		"GOMODCACHE=" + filepath.Join(home, "go-mod-cache"),
		"CARGO_HOME=" + filepath.Join(home, "cargo"),
		"RUSTUP_HOME=" + filepath.Join(home, "rustup"),
		"GRADLE_USER_HOME=" + filepath.Join(home, "gradle"),
		"NPM_CONFIG_CACHE=" + filepath.Join(home, "npm"),
		"GOPATH=" + filepath.Join(home, "go-a") + string(filepath.ListSeparator) + filepath.Join(home, "go-b"),
	}
	initial, err := prepareProfileEnvironment(values, true)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := prepareProfileEnvironment(initial.Values, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(initial.Values, reconstructed.Values) || !slices.Equal(initial.WritableRoots, reconstructed.WritableRoots) {
		t.Fatalf("native environment changed after round trip: values=%q/%q roots=%q/%q", initial.Values, reconstructed.Values, initial.WritableRoots, reconstructed.WritableRoots)
	}
}
