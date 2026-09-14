package antigravity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestEffectivePolicyTracksSourceAndMembershipDrift(t *testing.T) {
	home, workspace := profileFixture(t)
	first := resolveFixturePolicy(t, home, workspace)
	if first.Policy == nil || first.Policy.ProfileRevision != ProfileRevision || len(first.Policy.Sources) == 0 ||
		first.Approval != "accept-edits:request-review:headless-deny" {
		t.Fatal("normalized policy decisions were not retained")
	}
	if !task.CompareEffectiveConfigs(first, resolveFixturePolicy(t, home, workspace)) {
		t.Fatal("unchanged policy produced a different snapshot")
	}
	writeFixtureSettings(t, home, workspace, "changed-model")
	second := resolveFixturePolicy(t, home, workspace)
	if task.CompareEffectiveConfigs(first, second) || first.Digest == second.Digest {
		t.Fatal("source change was not bound into effective policy")
	}
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	third := resolveFixturePolicy(t, home, workspace)
	if task.CompareEffectiveConfigs(second, third) || second.Digest == third.Digest {
		t.Fatal("directory creation was not bound into effective policy")
	}
}

func TestEffectivePolicyRejectsProjectOverridesAndUntrustedWorkspace(t *testing.T) {
	home, workspace := profileFixture(t)
	project := filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json")
	writeSourceTestFile(t, project, []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{},"allowNonWorkspaceAccess":true}`))
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	environment := fixtureProfileEnvironment(t, home)
	if _, err = resolveEffectivePolicy(fixtureRuntime(), environment, inventory); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("project override accepted: %v", err)
	}
	if err = validateWorkspaceTrust([]string{home}, workspace); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("unrelated trust root accepted: %v", err)
	}
	if err = validateWorkspaceTrust([]string{filepath.Dir(workspace)}, workspace); err != nil {
		t.Fatalf("canonical ancestor candidate refused before native verification: %v", err)
	}
}

func TestEnvironmentPreservesCredentialsWithoutPersistingThem(t *testing.T) {
	home, workspace := profileFixture(t)
	const secret = "fixture-secret-not-to-be-persisted"
	values := []string{"HOME=" + home, "GEMINI_API_KEY=" + secret}
	environment, err := prepareEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	values[1] = "GEMINI_API_KEY=changed"
	if environment.Values[1] != "GEMINI_API_KEY="+secret {
		t.Fatal("prepared environment retained a mutable caller alias")
	}
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolveEffectivePolicy(fixtureRuntime(), environment, inventory)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "GEMINI_API_KEY") {
		t.Fatal("credential environment entered persisted policy")
	}
}

func TestEnvironmentRejectsDiscoveryOverridesAndMalformedRoots(t *testing.T) {
	home, _ := profileFixture(t)
	for _, extra := range []string{"AGY_HOME=/other", "XDG_CONFIG_HOME=/other", "TMPDIR=relative", "GOCACHE=relative", "GOCACHE=OFF", "GOCACHE=off/", "HOME=/duplicate", "invalid"} {
		if _, err := prepareEnvironment([]string{"HOME=" + home, extra}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Errorf("unsupported environment %q accepted: %v", extra, err)
		}
	}
}

func TestEnvironmentAcceptsDisabledGoCacheSentinel(t *testing.T) {
	home, _ := profileFixture(t)
	environment, err := prepareEnvironment([]string{"HOME=" + home, "GOCACHE=off"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(environment.WritableRoots, "off") {
		t.Fatal("GOCACHE=off was recorded as a writable root")
	}
}

func TestEffectivePolicyNormalizesExplicitEmptyToolPermission(t *testing.T) {
	home, workspace := profileFixture(t)
	settings, err := json.Marshal(map[string]any{
		"model":             "initial-model",
		"trustedWorkspaces": []string{workspace},
		"toolPermission":    "",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), settings)
	effective := resolveFixturePolicy(t, home, workspace)
	if effective.Approval != "accept-edits:request-review:headless-deny" {
		t.Fatalf("effective approval = %q", effective.Approval)
	}
}

func profileFixture(t *testing.T) (string, string) {
	t.Helper()
	base := sourceTestRoot(t)
	home, workspace := filepath.Join(base, "home"), filepath.Join(base, "workspace")
	for _, path := range []string{workspace, filepath.Join(home, ".gemini", "config", "projects"), filepath.Join(home, ".gemini", "antigravity-cli", "cache")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureSettings(t, home, workspace, "initial-model")
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "config", "config.json"), []byte(`{"userSettings":{"remoteControlHostname":"fixture"}}`))
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json"), []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{}}`))
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", "default_project_id.txt"), []byte(defaultProjectID))
	return home, workspace
}

func writeFixtureSettings(t *testing.T, home, workspace, model string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"model": model, "trustedWorkspaces": []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), encoded)
}

func fixtureRuntime() runtimeIdentity {
	return runtimeIdentity{Executable: "/fixture/agy", Version: Version, SHA256: task.ComputeSHA256([]byte("fixture runtime")), OS: "darwin", Arch: "arm64"}
}

func fixtureProfileEnvironment(t *testing.T, home string) profileEnvironment {
	t.Helper()
	environment, err := prepareEnvironment([]string{"HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func resolveFixturePolicy(t *testing.T, home, workspace string) task.EffectiveConfig {
	t.Helper()
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolveEffectivePolicy(fixtureRuntime(), fixtureProfileEnvironment(t, home), inventory)
	if err != nil {
		t.Fatal(err)
	}
	return effective
}
