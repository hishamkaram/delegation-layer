package antigravity

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestNativePreparationLeavesProviderConfigurationAlone(t *testing.T) {
	home, workspace := profileFixture(t)
	executable := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(executable))
	t.Setenv("XDG_DATA_DIRS", "/usr/local/share:/usr/share")
	t.Setenv("GEMINI_API_KEY", "fixture-secret")
	configPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	content := []byte(`{"mcpServers":{"example":{"command":"example"}}}`)
	writeSourceTestFile(t, configPath, content)
	request := argumentRequest()
	request.CanonicalCwd = workspace
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Effective.Policy.ProfileRevision != commonprovider.NativeProfileRevision || len(profile.Effective.Policy.Sources) != 0 {
		t.Fatal("native configuration was inventoried")
	}
	if slices.Contains(profile.Plan.Arguments, "--disable-slash-commands") {
		t.Fatal("native customization disabled")
	}
	if !slices.Contains(profile.Plan.Environment, "XDG_DATA_DIRS=/usr/local/share:/usr/share") || slices.Contains(profile.Plan.Environment, "GEMINI_API_KEY=fixture-secret") {
		t.Fatal("incorrect discovery or credential environment")
	}
	actual, err := os.ReadFile(configPath)
	if err != nil || string(actual) != string(content) {
		t.Fatal("provider configuration changed")
	}
	assertNativeReconstruction(t, request, profile, facts)
}

func assertNativeReconstruction(t *testing.T, request task.TaskRecord, profile commonprovider.PreparedProfile, facts []byte) {
	t.Helper()
	reconstructed, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: profile.Effective})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reconstructed.Finalize(facts, time.Now())
	if err != nil || !task.CompareEffectiveConfigs(profile.Effective, replay.Effective) {
		t.Fatal("native profile reconstruction changed")
	}
	if _, err = PrepareExistingCandidate(request, task.MetaRecord{}); err == nil {
		t.Fatal("historical configuration restrictions silently relaxed")
	}
}

func TestNativeEnvironmentReconstructsRuntimePaths(t *testing.T) {
	home := t.TempDir()
	initial, err := prepareProfileEnvironment([]string{"HOME=" + home, "GOPATH=" + filepath.Join(home, "custom-go"), "GOCACHE=" + filepath.Join(home, "custom-cache"), "XDG_DATA_DIRS=/usr/share", "XDG_DATA_HOME=" + filepath.Join(home, "native-data")}, true)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := prepareProfileEnvironment(initial.Values, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(initial.WritableRoots, reconstructed.WritableRoots) || !slices.Equal(initial.Values, reconstructed.Values) {
		t.Fatal("native runtime paths drifted after environment reconstruction")
	}
}
