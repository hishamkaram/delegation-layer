package pi

import (
	"path/filepath"
	"slices"
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
